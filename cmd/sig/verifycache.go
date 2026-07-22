// Verify-result cache (issue #18, -verify-cache): skip re-running -verify
// when the exact same (tree OID, resolved verify command) pair has already
// been proven to pass, for THIS build of sigbound.
//
// A cache hit must NEVER weaken the -verify gate, so the design is fail-safe
// by construction:
//
//   - Keyed on the TREE OID of the checked-out commit, not the commit SHA —
//     git trees are content-addressed, so two different commits (a fresh
//     integration vs. a resumed/replayed one) that land the exact same
//     content hit the same entry, which is the whole point (see the issue).
//   - The key also folds in a hash of the exact command that would run
//     (verifyCacheCmdHash) — -verify-impact composes correctly for free,
//     since runVerify already resolves to the impact command + package list
//     BEFORE the cache is consulted, so a scoped run and a full run over the
//     same tree never collide — and the running sigbound Version, so an
//     upgrade never serves a verdict computed under different semantics.
//   - ONLY a PASS is ever cached (verifyCacheStore is only called after a
//     zero exit). A flaky environment must never pin a red as a permanently
//     cached failure, and caching a NO-verdict risks staleness in a way a
//     cached YES never can: fail-safe wins, so a miss just costs a real
//     re-run, never a wrong answer served early.
//   - Storage lives under the TARGET repo's shared .git dir (see
//     gitx.Git.GitCommonDir), never the working tree: `rm -rf
//     .git/sigbound` is the documented full reset, and it never shows up in
//     `git status`. No eviction — entries are a few hundred bytes each.
//
// See runVerify for where this is consulted/populated and docs/USAGE.md's
// Cache section for the user-facing contract.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// verifyCacheEntry is the on-disk JSON value for one cache entry. OK is
// always true (see the package doc: only passes are ever written), kept as
// an explicit field rather than the presence of the file alone so a
// corrupted/truncated write is caught by JSON parsing instead of read as a
// silent pass. CmdHash duplicates part of what the filename (the composite
// key) already encodes — cheap belt-and-suspenders against ever trusting a
// filesystem collision.
type verifyCacheEntry struct {
	OK      bool      `json:"ok"`
	TS      time.Time `json:"ts"`
	Version string    `json:"version"`
	CmdHash string    `json:"cmdHash"`
}

// verifyCacheCmdHash hashes the exact command that would run plus, when
// -verify-impact scoped it, the impacted-package list — so a full run and a
// scoped run over the identical tree never share an entry, and a change in
// which packages were deemed impacted (a different landed write-set)
// naturally misses instead of serving a stale scope's verdict.
func verifyCacheCmdHash(resolvedCmd string, impactedPkgs []string) string {
	h := sha256.New()
	io.WriteString(h, resolvedCmd)
	h.Write([]byte{0})
	io.WriteString(h, strings.Join(impactedPkgs, "\x1f"))
	return hex.EncodeToString(h.Sum(nil))
}

// verifyCacheKey composes the full cache key: tree OID + cmdHash + the
// running sigbound Version, so a rebuilt/upgraded binary never trusts a
// verdict cached under different semantics.
func verifyCacheKey(treeOID, cmdHash string) string {
	h := sha256.New()
	io.WriteString(h, treeOID)
	h.Write([]byte{0})
	io.WriteString(h, cmdHash)
	h.Write([]byte{0})
	io.WriteString(h, Version)
	return hex.EncodeToString(h.Sum(nil))
}

// verifyCacheDir resolves -verify-cache's storage directory:
// <target repo's shared .git dir>/sigbound/verify-cache. Resolved via git
// (GitCommonDir) rather than a bare filepath.Join(repo, ".git") so it's
// correct even when -repo is itself a linked worktree.
func verifyCacheDir(ctx context.Context, g *gitx.Git) (string, error) {
	common, err := g.GitCommonDir(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve git dir for -verify-cache: %w", err)
	}
	return filepath.Join(common, "sigbound", "verify-cache"), nil
}

// verifyCacheLookup reports whether (treeOID, resolvedCmd, impactedPkgs) has
// a cached PASS for this build. Any trouble at all — the cache dir can't be
// resolved, the entry is missing, unparsable, or was written by a different
// version/command — is treated as a miss: caching must only ever save a
// redundant re-run, never risk serving a wrong verdict.
func verifyCacheLookup(ctx context.Context, g *gitx.Git, treeOID, resolvedCmd string, impactedPkgs []string) bool {
	dir, err := verifyCacheDir(ctx, g)
	if err != nil {
		return false
	}
	cmdHash := verifyCacheCmdHash(resolvedCmd, impactedPkgs)
	data, err := os.ReadFile(filepath.Join(dir, verifyCacheKey(treeOID, cmdHash)))
	if err != nil {
		return false
	}
	var e verifyCacheEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return false
	}
	return e.OK && e.Version == Version && e.CmdHash == cmdHash
}

// verifyCacheStore records a PASS for (treeOID, resolvedCmd, impactedPkgs).
// Callers must only invoke this after a verify command actually exited 0 —
// see the package doc for why a failure is never cached. Best-effort: a
// write failure here (a read-only .git, a full disk, ...) must never turn a
// verify run that genuinely just passed into a reported failure, so every
// error is swallowed.
func verifyCacheStore(ctx context.Context, g *gitx.Git, treeOID, resolvedCmd string, impactedPkgs []string) {
	dir, err := verifyCacheDir(ctx, g)
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	cmdHash := verifyCacheCmdHash(resolvedCmd, impactedPkgs)
	data, err := json.Marshal(verifyCacheEntry{OK: true, TS: time.Now().UTC(), Version: Version, CmdHash: cmdHash})
	if err != nil {
		return
	}
	key := verifyCacheKey(treeOID, cmdHash)
	// Write-then-rename: a concurrent `sig run` verifying the same tree at
	// the same time must never observe a torn/partial entry.
	tmp := filepath.Join(dir, "."+key+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, filepath.Join(dir, key)); err != nil {
		os.Remove(tmp)
	}
}
