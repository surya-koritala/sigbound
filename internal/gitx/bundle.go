package gitx

// This file wraps git's native bundle transport — the on-top-of-git, host-free
// way to physically MOVE objects between repos. A bundle is one ordinary file
// (it rides any scp/copy/artifact-store get); there is no server and no custom
// protocol. It is the object-transport substrate under cell.Export/Import (#59):
// a worker bundles its agent branches, the file moves by whatever means the user
// has, and a coordinator verifies + unbundles it into an isolated namespace.
//
// The three primitives map 1:1 to git's own: create, verify, unbundle. The one
// parser of untrusted git output here (parseUnbundleRefs) is fuzzed, per the
// repo rule for every decoder of external command output.

import (
	"context"
	"fmt"
	"strings"
)

// BundleRef is one ref a bundle carries: the object OID and its full ref name
// (e.g. refs/heads/agent/t1), exactly as `git bundle unbundle` reports it on
// stdout.
type BundleRef struct {
	OID string
	Ref string
}

// BundleCreate writes a git bundle at path carrying refs (`git bundle create
// <path> <refs...>`). Each ref is bundled with its COMPLETE history (no basis),
// so the file unbundles cleanly into any repo regardless of what objects it
// already holds — there are no prerequisite commits to satisfy. git refuses to
// create an empty bundle, so refs must be non-empty; callers (cell.Export)
// validate the refs exist first, turning git's raw "ambiguous argument" into a
// clean error naming the missing branches.
func (g *Git) BundleCreate(ctx context.Context, path string, refs []string) error {
	if len(refs) == 0 {
		return fmt.Errorf("bundle create: no refs")
	}
	// `--` guards the path against option injection (design rule); refs follow
	// as rev-list args, already validated by the caller.
	args := append([]string{"bundle", "create", "--", path}, refs...)
	_, err := g.run(ctx, args...)
	return err
}

// BundleVerify runs `git bundle verify <path>` — REQUIRED before any unbundle.
// It validates the bundle HEADER (it really is a v2/v3 bundle) and that this
// repo already contains the bundle's prerequisite commits, failing loudly on a
// non-bundle file, a header-corrupt file, or a missing prerequisite. It does
// NOT hash the packfile body, so a bundle whose header is intact but whose pack
// is truncated still passes here and instead dies loudly at BundleUnbundle
// (index-pack) — the second gate. Either failure means nothing is imported.
// git requires a repository context for verify, which g always has (-C dir).
func (g *Git) BundleVerify(ctx context.Context, path string) error {
	_, err := g.run(ctx, "bundle", "verify", "--", path)
	return err
}

// BundleUnbundle imports a bundle's objects into this repo and returns the refs
// it carried, parsed from `git bundle unbundle` stdout. Crucially it writes NO
// refs of its own — it only imports objects and prints "<oid> <ref>" lines — so
// the caller maps the returned refs wherever it wants. That is exactly what lets
// cell.Import land them under an isolated namespace and NEVER move an existing
// ref (main, agent/*): a bundle carrying "refs/heads/main" cannot touch this
// repo's main here. `git index-pack` validates the pack, so a truncated/corrupt
// pack fails loudly rather than importing partial objects.
func (g *Git) BundleUnbundle(ctx context.Context, path string) ([]BundleRef, error) {
	out, err := g.run(ctx, "bundle", "unbundle", "--", path)
	if err != nil {
		return nil, err
	}
	return parseUnbundleRefs(out)
}

// parseUnbundleRefs decodes `git bundle unbundle` stdout: one "<oid> <ref>" line
// per ref the bundle carried (progress, when enabled, goes to stderr, never
// here). Git refnames never contain a space, so splitting on the first space is
// exact. It is a pure function of untrusted git output and must never panic; a
// line that is not a well-formed oid+ref pair is rejected as an error (the
// caller surfaces it) rather than guessed at, so a bad bundle can never map a
// garbage ref. Fuzzed by FuzzParseUnbundleRefs.
func parseUnbundleRefs(out string) ([]BundleRef, error) {
	var refs []BundleRef
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			return nil, fmt.Errorf("bundle unbundle: malformed line %q", line)
		}
		oid := line[:sp]
		ref := strings.TrimSpace(line[sp+1:])
		if !isHexOID(oid) || ref == "" || strings.ContainsAny(ref, " \t") {
			return nil, fmt.Errorf("bundle unbundle: malformed line %q", line)
		}
		refs = append(refs, BundleRef{OID: oid, Ref: ref})
	}
	return refs, nil
}

// isHexOID reports whether s is a git object id: 40 hex chars (sha1) or 64
// (sha256), nothing else.
func isHexOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}
