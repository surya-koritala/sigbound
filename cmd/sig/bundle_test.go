package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// wrote — runIntegrate prints its JSON straight to os.Stdout, so this is how the
// composition step's output is inspected.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}

// TestExportImportIntegrateCLI is the CLI-level round-trip: a worker exports two
// agent branches; a coordinator (a separate repo seeded with only the base)
// imports them under imported/w1/; then the EXISTING `sig integrate` lands the
// imported branches — proving the transport composes with the normal engine, no
// new integration path.
func TestExportImportIntegrateCLI(t *testing.T) {
	ctx := context.Background()

	// Worker A: base + two disjoint agent branches.
	gA, base := gitRepoWithGoFile(t, "", map[string]string{"base.txt": "base\n"})
	mkBranchFrom(t, gA, "agent/t1", base, map[string]string{"t1.txt": "t1\n"})
	mkBranchFrom(t, gA, "agent/t2", base, map[string]string{"t2.txt": "t2\n"})

	// sig export -json.
	bundle := filepath.Join(t.TempDir(), "work.bundle")
	var exp bytes.Buffer
	if err := runExport(&exp, []string{"-repo", gA.Dir(), "-bundle", bundle, "-branches", "agent/t1,agent/t2", "-json"}); err != nil {
		t.Fatalf("runExport: %v\n%s", err, exp.String())
	}
	var ej exportJSON
	if err := json.Unmarshal(exp.Bytes(), &ej); err != nil {
		t.Fatalf("parse export json: %v\n%s", err, exp.String())
	}
	if len(ej.Branches) != 2 || ej.Bundle != bundle {
		t.Fatalf("export json = %+v", ej)
	}

	// Coordinator B: fresh repo seeded with ONLY the base (via a base bundle).
	baseBundle := filepath.Join(t.TempDir(), "base.bundle")
	if err := runExport(io.Discard, []string{"-repo", gA.Dir(), "-bundle", baseBundle, "-branches", "main"}); err != nil {
		t.Fatalf("export base: %v", err)
	}
	bDir := filepath.Join(t.TempDir(), "coord")
	gB := gitx.New(bDir)
	if err := gB.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := gB.BundleUnbundle(ctx, baseBundle); err != nil {
		t.Fatal(err)
	}
	if err := gB.UpdateRef(ctx, "refs/heads/main", base); err != nil {
		t.Fatal(err)
	}

	// sig import -json into B under worker id w1.
	var imp bytes.Buffer
	if err := runImport(&imp, []string{"-repo", bDir, "-bundle", bundle, "-from", "w1", "-json"}); err != nil {
		t.Fatalf("runImport: %v\n%s", err, imp.String())
	}
	var ij importResultJSON
	if err := json.Unmarshal(imp.Bytes(), &ij); err != nil {
		t.Fatalf("parse import json: %v\n%s", err, imp.String())
	}
	if len(ij.Imported) != 2 {
		t.Fatalf("imported = %+v, want 2", ij.Imported)
	}
	var branches []string
	for _, ib := range ij.Imported {
		if !strings.HasPrefix(ib.Ref, "refs/heads/imported/w1/") {
			t.Fatalf("imported ref %q escaped the namespace", ib.Ref)
		}
		branches = append(branches, strings.TrimPrefix(ib.Ref, "refs/heads/"))
	}

	// sig integrate the imported branches (the EXISTING command, unchanged).
	out := captureStdout(t, func() {
		if err := runIntegrate([]string{
			"-repo", bDir, "-base", "main", "-branches", strings.Join(branches, ","), "-strategy", "overlay",
		}); err != nil {
			t.Fatalf("runIntegrate: %v", err)
		}
	})
	var res resultJSON
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse integrate json: %v\n%s", err, out)
	}
	if len(res.Landed) != 2 || len(res.Flagged) != 0 {
		t.Fatalf("integrate landed=%v flagged=%v, want both imported branches landed", res.Landed, res.Flagged)
	}

	// The coordinator's main now carries both imported agents' files.
	files, err := gB.LsTree(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, f := range files {
		have[f] = true
	}
	if !have["t1.txt"] || !have["t2.txt"] {
		t.Fatalf("coordinator main missing imported files after integrate: %v", files)
	}
}

// TestBundleCLIErrors covers the required-flag and default-worker-id surface.
func TestBundleCLIErrors(t *testing.T) {
	if err := runExport(io.Discard, []string{"-bundle", "x.bundle", "-branches", "a"}); err == nil {
		t.Fatal("export without -repo: want error")
	}
	if err := runExport(io.Discard, []string{"-repo", ".", "-branches", "a"}); err == nil {
		t.Fatal("export without -bundle: want error")
	}
	if err := runExport(io.Discard, []string{"-repo", ".", "-bundle", "x.bundle"}); err == nil {
		t.Fatal("export without -branches: want error")
	}
	if err := runImport(io.Discard, []string{"-bundle", "x.bundle"}); err == nil {
		t.Fatal("import without -repo: want error")
	}
	if err := runImport(io.Discard, []string{"-repo", "."}); err == nil {
		t.Fatal("import without -bundle: want error")
	}
}

// TestImportDefaultsWorkerFromFilename proves the -from default is the bundle
// filename stem.
func TestImportDefaultsWorkerFromFilename(t *testing.T) {
	ctx := context.Background()
	gA, base := gitRepoWithGoFile(t, "", map[string]string{"base.txt": "base\n"})
	mkBranchFrom(t, gA, "agent/t1", base, map[string]string{"t1.txt": "t1\n"})
	bundle := filepath.Join(t.TempDir(), "nodeX.bundle")
	if err := runExport(io.Discard, []string{"-repo", gA.Dir(), "-bundle", bundle, "-branches", "agent/t1"}); err != nil {
		t.Fatal(err)
	}
	var imp bytes.Buffer
	// No -from: worker id defaults to "nodeX" (the filename stem).
	if err := runImport(&imp, []string{"-repo", gA.Dir(), "-bundle", bundle, "-json"}); err != nil {
		t.Fatalf("runImport: %v\n%s", err, imp.String())
	}
	var ij importResultJSON
	if err := json.Unmarshal(imp.Bytes(), &ij); err != nil {
		t.Fatal(err)
	}
	if len(ij.Imported) != 1 || !strings.HasPrefix(ij.Imported[0].Ref, "refs/heads/imported/nodeX/") {
		t.Fatalf("default worker id not derived from filename: %+v", ij.Imported)
	}
	_ = ctx
}
