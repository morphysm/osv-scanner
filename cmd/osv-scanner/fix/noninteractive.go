package fix

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"deps.dev/util/resolve"
	"github.com/google/osv-scanner/internal/remediation"
	"github.com/google/osv-scanner/internal/resolution"
	"github.com/google/osv-scanner/internal/resolution/client"
	lf "github.com/google/osv-scanner/internal/resolution/lockfile"
	"github.com/google/osv-scanner/internal/resolution/manifest"
	"github.com/google/osv-scanner/pkg/lockfile"
	"github.com/google/osv-scanner/pkg/reporter"
	"golang.org/x/exp/maps"
)

func autoInPlace(ctx context.Context, r reporter.Reporter, opts osvFixOptions, maxUpgrades int) error {
	if !remediation.SupportsInPlace(opts.LockfileRW) {
		return errors.New("in-place strategy is not supported for lockfile")
	}

	r.Infof("Scanning %s...\n", opts.Lockfile)
	f, err := lockfile.OpenLocalDepFile(opts.Lockfile)
	if err != nil {
		return err
	}

	g, err := opts.LockfileRW.Read(f)
	f.Close()
	if err != nil {
		return err
	}

	res, err := remediation.ComputeInPlacePatches(ctx, opts.Client, g, opts.RemediationOptions)
	if err != nil {
		return err
	}

	patches, fixed, nRemain := autoChooseInPlacePatches(res, maxUpgrades)
	nFixed := len(fixed)
	totalVulns := nFixed + nRemain + len(res.Unfixable)

	r.Infof("Found %d vulnerabilities matching the filter\n", totalVulns)
	r.Infof("Can fix %d/%d matching vulnerabilities by changing %d dependencies\n", nFixed, totalVulns, len(patches))

	for _, p := range patches {
		r.Infof("UPGRADED-PACKAGE: %s,%s,%s\n", p.Pkg.Name, p.OrigVersion, p.NewVersion)
	}

	r.Infof("FIXED-VULN-IDS: ")
	for i, v := range fixed {
		if i > 0 {
			r.Infof(",")
		}
		r.Infof("%s", v.Vulnerability.ID)
	}
	r.Infof("\n")

	r.Infof("REMAINING-VULNS: %d\n", totalVulns-nFixed)
	r.Infof("UNFIXABLE-VULNS: %d\n", len(res.Unfixable))
	// TODO: Print the REMAINING-VULN-IDS, UNFIXABLE-VULN-IDS

	r.Infof("Rewriting %s...\n", opts.Lockfile)

	return lf.Overwrite(opts.LockfileRW, opts.Lockfile, patches)
}

// returns the top {maxUpgrades} compatible patches, the vulns fixed, and the number of potentially fixable vulns left unfixed
// if maxUpgrades is < 0, do as many patches as possible
func autoChooseInPlacePatches(res remediation.InPlaceResult, maxUpgrades int) ([]lf.DependencyPatch, []resolution.ResolutionVuln, int) {
	// Keep track of the VersionKeys we've already patched so we know which patches are incompatible
	seenVKs := make(map[resolve.VersionKey]bool)

	// Key vulnerabilities by (ID, package name, package version) to be consistent with scan action's counting
	type vulnKey struct {
		id string
		vk resolve.VersionKey
	}
	uniqueVulns := make(map[vulnKey]struct{})
	var patches []lf.DependencyPatch
	var fixed []resolution.ResolutionVuln

	for _, p := range res.Patches {
		vk := resolve.VersionKey{
			PackageKey: p.Pkg,
			Version:    p.OrigVersion,
		}

		// add each of the resolved vulnKeys to the set of unique vulns
		for _, rv := range p.ResolvedVulns {
			uniqueVulns[vulnKey{id: rv.Vulnerability.ID, vk: vk}] = struct{}{}
		}

		// If we still are picking more patches, and we haven't already patched this specific version,
		// then add this patch to our final set of patches and count the vulnerabilities
		if maxUpgrades != 0 && !seenVKs[vk] {
			seenVKs[vk] = true
			patches = append(patches, p.DependencyPatch)
			maxUpgrades--
			fixed = append(fixed, p.ResolvedVulns...)
		}
	}

	// Sort the fixed vulns by ID for consistency.
	slices.SortFunc(fixed, func(a, b resolution.ResolutionVuln) int { return cmp.Compare(a.Vulnerability.ID, b.Vulnerability.ID) })

	return patches, fixed, len(uniqueVulns) - len(fixed)
}

func autoRelock(ctx context.Context, r reporter.Reporter, opts osvFixOptions, maxUpgrades int) error {
	if !remediation.SupportsRelax(opts.ManifestRW) {
		return errors.New("relock strategy is not supported for manifest")
	}

	r.Infof("Resolving %s...\n", opts.Manifest)
	f, err := lockfile.OpenLocalDepFile(opts.Manifest)
	if err != nil {
		return err
	}

	manif, err := opts.ManifestRW.Read(f)
	f.Close()
	if err != nil {
		return err
	}

	client.PreFetch(opts.Client, ctx, manif.Requirements, manif.FilePath)
	res, err := resolution.Resolve(ctx, opts.Client, manif, opts.ResolveOpts)
	if err != nil {
		return err
	}

	if errs := res.Errors(); len(errs) > 0 {
		r.Warnf("WARNING: encountered %d errors during dependency resolution:\n", len(errs))
		r.Warnf("%s", resolutionErrorString(res, errs))
	}

	res.FilterVulns(opts.MatchVuln)
	// TODO: count vulnerabilities per unique version as scan action does
	totalVulns := len(res.Vulns)
	r.Infof("Found %d vulnerabilities matching the filter\n", totalVulns)

	allPatches, err := remediation.ComputeRelaxPatches(ctx, opts.Client, res, opts.RemediationOptions)
	if err != nil {
		return err
	}

	if err := opts.Client.WriteCache(manif.FilePath); err != nil {
		r.Warnf("WARNING: failed to write resolution cache: %v\n", err)
	}

	if len(allPatches) == 0 {
		r.Infof("No dependency patches are possible\n")
		r.Infof("REMAINING-VULNS: %d\n", totalVulns)
		r.Infof("UNFIXABLE-VULNS: %d\n", totalVulns)

		return nil
	}

	depPatches, fixed := autoChooseRelockPatches(allPatches, maxUpgrades)
	nFixed := len(fixed)
	nUnfixable := len(relockUnfixableVulns(allPatches))
	r.Infof("Can fix %d/%d matching vulnerabilities by changing %d dependencies\n", nFixed, totalVulns, len(depPatches))
	for _, p := range depPatches {
		r.Infof("UPGRADED-PACKAGE: %s,%s,%s\n", p.Pkg.Name, p.OrigRequire, p.NewRequire)
	}

	r.Infof("FIXED-VULN-IDS: ")
	for i, v := range fixed {
		if i > 0 {
			r.Infof(",")
		}
		r.Infof("%s", v.Vulnerability.ID)
	}
	r.Infof("\n")

	r.Infof("REMAINING-VULNS: %d\n", totalVulns-nFixed)
	r.Infof("UNFIXABLE-VULNS: %d\n", nUnfixable)
	// TODO: Print the REMAINING-VULN-IDS, UNFIXABLE-VULN-IDS
	// TODO: Consider potentially introduced vulnerabilities

	r.Infof("Rewriting %s...\n", opts.Manifest)
	if err := manifest.Overwrite(opts.ManifestRW, opts.Manifest, manifest.ManifestPatch{Manifest: &manif, Deps: depPatches}); err != nil {
		return err
	}

	if opts.Lockfile != "" || opts.RelockCmd != "" {
		// We only recreate the lockfile if we know a lockfile already exists
		// or we've been given a command to run.
		r.Infof("Shelling out to regenerate lockfile...\n")
		cmd, err := regenerateLockfileCmd(opts)
		if err != nil {
			return err
		}
		// ideally I'd have the reporter's stdout/stderr here...
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		r.Infof("Executing `%s`...\n", cmd)
		err = cmd.Run()
		if err == nil {
			return nil
		}
		if opts.RelockCmd != "" {
			return err
		}
		r.Warnf("Install failed. Trying again with `--legacy-peer-deps`...\n")
		cmd, err = regenerateLockfileCmd(opts)
		if err != nil {
			return err
		}
		cmd.Args = append(cmd.Args, "--legacy-peer-deps")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		return cmd.Run()
	}

	return nil
}

// returns the top {maxUpgrades} compatible patches, and the vulns fixed
// if maxUpgrades is < 0, do as many patches as possible
func autoChooseRelockPatches(diffs []resolution.ResolutionDiff, maxUpgrades int) ([]manifest.DependencyPatch, []resolution.ResolutionVuln) {
	var patches []manifest.DependencyPatch
	pkgChanged := make(map[resolve.VersionKey]bool) // dependencies we've already applied a patch to
	var fixed []resolution.ResolutionVuln

	for _, diff := range diffs {
		// If we are not picking any more patches, or this patch is incompatible with existing patches, skip adding it to the patch list.
		// A patch is incompatible if any of its changed packages have already been changed by an existing patch.
		if maxUpgrades == 0 || slices.ContainsFunc(diff.Deps, func(dp manifest.DependencyPatch) bool {
			return pkgChanged[resolve.VersionKey{PackageKey: dp.Pkg, Version: dp.OrigRequire}]
		}) {
			continue
		}

		// Add all individual package patches to the final patch list, and add the vulns this is anticipated to resolve
		fixed = append(fixed, diff.RemovedVulns...)
		for _, dp := range diff.Deps {
			patches = append(patches, dp)
			pkgChanged[resolve.VersionKey{PackageKey: dp.Pkg, Version: dp.OrigRequire}] = true
		}
		maxUpgrades--
	}

	// Sort the fixed vulns by ID for consistency.
	slices.SortFunc(fixed, func(a, b resolution.ResolutionVuln) int { return cmp.Compare(a.Vulnerability.ID, b.Vulnerability.ID) })

	return patches, fixed
}

func relockUnfixableVulns(diffs []resolution.ResolutionDiff) []*resolution.ResolutionVuln {
	if len(diffs) == 0 {
		return nil
	}
	// find every vuln ID fixed in any patch
	fixableVulnIDs := make(map[string]struct{})
	for _, diff := range diffs {
		for _, v := range diff.RemovedVulns {
			fixableVulnIDs[v.Vulnerability.ID] = struct{}{}
		}
	}

	// select only vulns that aren't fixed in any patch
	var unfixable []*resolution.ResolutionVuln
	for i, v := range diffs[0].Original.Vulns {
		if _, ok := fixableVulnIDs[v.Vulnerability.ID]; !ok {
			unfixable = append(unfixable, &diffs[0].Original.Vulns[i])
		}
	}

	return unfixable
}

func autoOverride(ctx context.Context, r reporter.Reporter, opts osvFixOptions, maxUpgrades int) error {
	if !remediation.SupportsOverride(opts.ManifestRW) {
		return errors.New("override strategy is not supported for manifest")
	}

	r.Infof("Resolving %s...\n", opts.Manifest)
	f, err := lockfile.OpenLocalDepFile(opts.Manifest)
	if err != nil {
		return err
	}

	manif, err := opts.ManifestRW.Read(f)
	f.Close()
	if err != nil {
		return err
	}

	client.PreFetch(opts.Client, ctx, manif.Requirements, manif.FilePath)
	res, err := resolution.Resolve(ctx, opts.Client, manif, opts.ResolveOpts)
	if err != nil {
		return err
	}

	if errs := res.Errors(); len(errs) > 0 {
		r.Warnf("WARNING: encountered %d errors during dependency resolution:\n", len(errs))
		r.Warnf("%s", resolutionErrorString(res, errs))
	}

	res.FilterVulns(opts.MatchVuln)
	// TODO: count vulnerabilities per unique version as scan action does
	totalVulns := len(res.Vulns)
	r.Infof("Found %d vulnerabilities matching the filter\n", totalVulns)

	allPatches, err := remediation.ComputeOverridePatches(ctx, opts.Client, res, opts.RemediationOptions)
	if err != nil {
		return err
	}

	if err := opts.Client.WriteCache(manif.FilePath); err != nil {
		r.Warnf("WARNING: failed to write resolution cache: %v\n", err)
	}

	if len(allPatches) == 0 {
		r.Infof("No dependency patches are possible\n")
		r.Infof("REMAINING-VULNS: %d\n", totalVulns)
		r.Infof("UNFIXABLE-VULNS: %d\n", totalVulns)

		return nil
	}

	depPatches, fixed := autoChooseOverridePatches(allPatches, maxUpgrades)
	nFixed := len(fixed)
	nUnfixable := len(relockUnfixableVulns(allPatches))
	r.Infof("Can fix %d/%d matching vulnerabilities by overriding %d dependencies\n", nFixed, totalVulns, len(depPatches))
	for _, p := range depPatches {
		r.Infof("OVERRIDE-PACKAGE: %s,%s\n", p.Pkg.Name, p.NewRequire)
	}

	r.Infof("FIXED-VULN-IDS: ")
	for i, v := range fixed {
		if i > 0 {
			r.Infof(",")
		}
		r.Infof("%s", v.Vulnerability.ID)
	}
	r.Infof("\n")

	r.Infof("REMAINING-VULNS: %d\n", totalVulns-nFixed)
	r.Infof("UNFIXABLE-VULNS: %d\n", nUnfixable)
	// TODO: Print the FIXED-VULN-IDS, REMAINING-VULN-IDS, UNFIXABLE-VULN-IDS
	// TODO: Consider potentially introduced vulnerabilities

	r.Infof("Rewriting %s...\n", opts.Manifest)
	if err := manifest.Overwrite(opts.ManifestRW, opts.Manifest, manifest.ManifestPatch{Manifest: &manif, Deps: depPatches}); err != nil {
		return err
	}

	return nil
}

func autoChooseOverridePatches(diffs []resolution.ResolutionDiff, maxUpgrades int) ([]manifest.DependencyPatch, []resolution.ResolutionVuln) {
	if maxUpgrades == 0 {
		return nil, nil
	}

	var patches []manifest.DependencyPatch
	pkgChanged := make(map[resolve.PackageKey]bool)          // dependencies we've already applied a patch to
	fixedVulns := make(map[string]resolution.ResolutionVuln) // vulns that have already been fixed by a patch
	for _, diff := range diffs {
		// If this patch is incompatible with existing patches, skip adding it to the patch list.

		// A patch is incompatible if any of its changed packages have already been changed by an existing patch.
		if slices.ContainsFunc(diff.Deps, func(dp manifest.DependencyPatch) bool { return pkgChanged[dp.Pkg] }) {
			continue
		}
		// A patch is also incompatible if any fixed vulnerability has already been fixed by another patch.
		// This would happen if updating the version of one package has a side effect of also updating or removing one of its vulnerable dependencies.
		// e.g. We have {foo@1 -> bar@1}, and two possible patches [foo@3, bar@2].
		// Patching foo@3 makes {foo@3 -> bar@3}, which also fixes the vulnerability in bar.
		// Applying both patches would force {foo@3 -> bar@2}, which is less desirable.
		if slices.ContainsFunc(diff.RemovedVulns, func(rv resolution.ResolutionVuln) bool { _, ok := fixedVulns[rv.Vulnerability.ID]; return ok }) {
			continue
		}

		// Add all individual package patches to the final patch list, and track the vulns this is anticipated to fix.
		for _, dp := range diff.Deps {
			patches = append(patches, dp)
			pkgChanged[dp.Pkg] = true
		}
		for _, rv := range diff.RemovedVulns {
			fixedVulns[rv.Vulnerability.ID] = rv
		}

		maxUpgrades--
		if maxUpgrades == 0 {
			break
		}
	}

	// Sort the fixed vulns by ID for consistency.
	fixed := maps.Values(fixedVulns)
	slices.SortFunc(fixed, func(a, b resolution.ResolutionVuln) int { return cmp.Compare(a.Vulnerability.ID, b.Vulnerability.ID) })

	return patches, fixed
}

func resolutionErrorString(res *resolution.ResolutionResult, errs []resolution.ResolutionError) string {
	// we pass in the []ResolutionErrors because calling res.Errors() is costly
	s := strings.Builder{}
	for _, e := range errs {
		node := res.Graph.Nodes[e.NodeID]
		fmt.Fprintf(&s, "Error when resolving %s@%s:\n", node.Version.Name, node.Version.Version)
		req := e.Error.Req
		if strings.Contains(req.Version, ":") {
			// this will be the case with unsupported npm requirements e.g. `file:...`, `git+https://...`
			// TODO: don't rely on resolution to propagate these errors
			// No easy access to the `knownAs` field to find which package this corresponds to
			fmt.Fprintf(&s, "\tSkipped resolving unsupported version specification: %s\n", req.Version)
		} else {
			fmt.Fprintf(&s, "\t%v: %s@%s\n", e.Error.Error, req.Name, req.Version)
		}
	}

	return s.String()
}
