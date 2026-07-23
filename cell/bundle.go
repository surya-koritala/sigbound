package cell

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// This file is the distributed-transport seam (#59): a WORKER cell Exports its
// agent branches as a git bundle (one file, no server), and a COORDINATOR cell
// Imports that bundle into an isolated namespace, ready for the SAME
// sig-integrate engine to fold and land. Networking is out of scope — the file
// moves by whatever means the user has (scp, shared dir, artifact store); these
// two methods only produce and consume it.

// importNamespace is the ref prefix every imported branch lands under. Import
// writes ONLY under refs/heads/imported/<worker>/… and nowhere else, so a bundle
// can never move this cell's main or clobber an agent/* ref no matter what refs
// it carries — the safety property Import guarantees (asserted in bundle_test).
const importNamespace = "refs/heads/imported/"

// ImportedBranch is one branch a bundle carried, landed under the import
// namespace of THIS cell.
type ImportedBranch struct {
	Original string // branch name as it was on the worker, e.g. "agent/t1"
	Ref      string // where it landed here, e.g. "refs/heads/imported/w1/agent/t1"
	SHA      string // commit OID
}

// Export writes a git bundle at bundlePath carrying branches, for a coordinator
// cell to Import. Branches are validated to exist FIRST, so a typo or a missing
// branch is a clean error naming it rather than git's raw "ambiguous argument".
// The bundle carries each branch's complete history, so it unbundles into any
// repo with no prerequisites to satisfy.
func (c *Cell) Export(ctx context.Context, bundlePath string, branches []string) error {
	if len(branches) == 0 {
		return fmt.Errorf("export: no branches")
	}
	// Resolve every branch in ONE cat-file --batch-check process (no rev-parse
	// fork per branch), distinguishing "missing branch" (!exists) from a real
	// git failure (err) so the caller sees the right error.
	br, err := c.git.NewBatchReader(ctx)
	if err != nil {
		return err
	}
	defer br.Close()
	var missing []string
	for _, b := range branches {
		_, _, _, ok, rerr := br.Resolve(b + "^{commit}")
		if rerr != nil {
			return fmt.Errorf("export: resolve %q: %w", b, rerr)
		}
		if !ok {
			missing = append(missing, b)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("export: branch(es) not found: %s", strings.Join(missing, ", "))
	}
	return c.git.BundleCreate(ctx, bundlePath, branches)
}

// Import verifies and unbundles bundlePath into THIS cell, mapping every ref the
// bundle carried into an isolated namespace: refs/heads/imported/<worker>/<original>.
// workerID defaults to the bundle filename stem when empty.
//
// Safety: Import NEVER touches a ref outside its namespace. `git bundle
// unbundle` writes no refs of its own (only objects), and the only refs Import
// creates are the namespaced ones — so a bundle that carries "main" or
// "agent/x" can neither move this cell's main nor clobber an existing agent/*
// branch. The bundle is verified before unbundling (a corrupt header / missing
// prerequisite fails loudly at verify; a corrupt pack fails loudly at unbundle),
// so a bad bundle imports nothing.
//
// Idempotency: re-importing the SAME bundle is a no-op re-run — the objects are
// already present and every namespaced ref is set to the same OID. Re-using a
// worker id with a DIFFERENT bundle updates that worker's namespace in place,
// still never escaping refs/heads/imported/<worker>/.
func (c *Cell) Import(ctx context.Context, bundlePath, workerID string) ([]ImportedBranch, error) {
	if workerID == "" {
		workerID = bundleStem(bundlePath)
	}
	if !workerIDSafe(workerID) {
		return nil, fmt.Errorf("import: unsafe worker id %q (allowed: letters, digits, '.', '_', '-'; not '.'/'..' )", workerID)
	}
	// Verify BEFORE unbundling — the required gate against a corrupt/forged
	// bundle. Only after it passes do we import objects.
	if err := c.git.BundleVerify(ctx, bundlePath); err != nil {
		return nil, fmt.Errorf("import: verify %s: %w", bundlePath, err)
	}
	carried, err := c.git.BundleUnbundle(ctx, bundlePath)
	if err != nil {
		return nil, fmt.Errorf("import: unbundle %s: %w", bundlePath, err)
	}

	prefix := importNamespace + workerID + "/"
	updates := make(map[string]string, len(carried))
	imported := make([]ImportedBranch, 0, len(carried))
	for _, r := range carried {
		// Strip refs/heads/ for a branch; a non-branch ref (a tag, say) is still
		// forced INSIDE the namespace by stripping a leading refs/, so it can
		// never collide with a real branch and never escapes the prefix.
		original := strings.TrimPrefix(r.Ref, "refs/heads/")
		original = strings.TrimPrefix(original, "refs/")
		ref := prefix + original
		updates[ref] = r.OID
		imported = append(imported, ImportedBranch{Original: original, Ref: ref, SHA: r.OID})
	}
	// One update-ref --stdin transaction, all under the namespace. update-ref
	// rejects any malformed composed ref loudly, so a hostile original name can
	// at worst fail the import — never write outside refs/heads/imported/<id>/.
	if err := c.git.UpdateRefs(ctx, updates); err != nil {
		return nil, fmt.Errorf("import: map refs: %w", err)
	}
	return imported, nil
}

// bundleStem derives the default worker id from a bundle path: its filename with
// the final extension removed (e.g. "/x/worker-a.bundle" -> "worker-a").
func bundleStem(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// workerIDSafe keeps the worker id a single safe ref component: it becomes part
// of refs/heads/imported/<id>/…, so it must not be empty, be "."/"..", or carry
// a slash, whitespace, or any character outside [A-Za-z0-9._-]. Mirrors cmd/sig's
// slugSafe (no regexp dependency).
func workerIDSafe(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}
