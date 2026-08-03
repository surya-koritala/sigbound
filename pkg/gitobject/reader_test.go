package gitobject

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type fixture struct {
	dir          string
	commit, tree string
	blob, empty  string
	body         []byte
}

func newFixture(t testing.TB, packed bool) fixture {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.name", "gitobject test")
	runGit(t, dir, "config", "user.email", "gitobject@example.test")
	body := bytes.Repeat([]byte("packed object content\n"), 256)
	if err := os.WriteFile(filepath.Join(dir, "source.txt"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "--", "source.txt", "empty.txt")
	runGit(t, dir, "commit", "-q", "-m", "fixture")
	f := fixture{
		dir: dir, body: body,
		commit: runGit(t, dir, "rev-parse", "HEAD"),
		tree:   runGit(t, dir, "rev-parse", "HEAD^{tree}"),
		blob:   runGit(t, dir, "rev-parse", "HEAD:source.txt"),
		empty:  runGit(t, dir, "rev-parse", "HEAD:empty.txt"),
	}
	if packed {
		runGit(t, dir, "gc", "--prune=now")
		if matches, err := filepath.Glob(filepath.Join(dir, ".git", "objects", "pack", "*.pack")); err != nil || len(matches) == 0 {
			t.Fatalf("fixture was not packed: matches=%v err=%v", matches, err)
		}
	}
	return f
}

func runGit(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = hermeticEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestReaderReadsLooseAndPackedObjectsWithSizeBeforeContent(t *testing.T) {
	for _, packed := range []bool{false, true} {
		t.Run(fmt.Sprintf("packed=%v", packed), func(t *testing.T) {
			f := newFixture(t, packed)
			r, err := Open(f.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()

			blob, _ := Exact(f.blob, Blob, true, int64(len(f.body)))
			empty, _ := Exact(f.empty, Blob, true, 0)
			commit, _ := Exact(f.commit, Commit, false, 0)
			tree, _ := Exact(f.tree, Tree, false, 0)
			results, err := r.Read(t.Context(), []Request{blob, empty, commit, tree}, int64(len(f.body)))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(results[0].Content, f.body) || results[0].Status != Available || results[0].Size != int64(len(f.body)) {
				t.Fatalf("blob result = status=%s size=%d content=%d", results[0].Status, results[0].Size, len(results[0].Content))
			}
			if results[1].Status != Available || results[1].Content == nil || len(results[1].Content) != 0 {
				t.Fatalf("empty blob must have non-nil empty content: %+v", results[1])
			}
			if results[2].Type != Commit || results[2].Content != nil || results[3].Type != Tree {
				t.Fatalf("metadata results = %+v / %+v", results[2], results[3])
			}
		})
	}
}

func TestReaderReadsDeltaCompressedPackedBlob(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.name", "gitobject test")
	runGit(t, dir, "config", "user.email", "gitobject@example.test")

	// Closely related versions give pack-objects an unambiguous delta chain.
	// We prove the chosen object is actually stored as a delta with verify-pack
	// before using the public reader, rather than merely proving a packed read.
	bodies := make(map[string][]byte)
	path := filepath.Join(dir, "delta.txt")
	base := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 2048)
	for i := 0; i < 16; i++ {
		body := append([]byte(nil), base...)
		copy(body[i*64:], fmt.Appendf(nil, "version-%04d", i))
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, dir, "add", "--", "delta.txt")
		runGit(t, dir, "commit", "-q", "-m", fmt.Sprintf("delta version %d", i))
		oid := runGit(t, dir, "rev-parse", "HEAD:delta.txt")
		bodies[oid] = body
	}
	runGit(t, dir, "repack", "-a", "-d", "-f", "--depth=50", "--window=50")
	indexes, err := filepath.Glob(filepath.Join(dir, ".git", "objects", "pack", "*.idx"))
	if err != nil || len(indexes) != 1 {
		t.Fatalf("packed index = %v, %v", indexes, err)
	}
	verify := runGit(t, dir, "verify-pack", "-v", indexes[0])
	var deltaOID string
	for _, line := range strings.Split(verify, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 7 {
			if _, ok := bodies[fields[0]]; ok {
				deltaOID = fields[0]
				break
			}
		}
	}
	if deltaOID == "" {
		t.Fatalf("verify-pack did not report a delta-compressed fixture blob:\n%s", verify)
	}

	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	want := bodies[deltaOID]
	request, _ := Exact(deltaOID, Blob, true, int64(len(want)))
	results, err := r.Read(t.Context(), []Request{request}, int64(len(want)))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != Available || !bytes.Equal(results[0].Content, want) {
		t.Fatalf("delta read = %+v", results)
	}
}

func TestReaderSupportsSHA256RepositoriesWhenGitDoes(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("git", "-C", dir, "init", "-q", "-b", "main", "--object-format=sha256")
	cmd.Env = hermeticEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("installed Git has no SHA-256 repository support: %v: %s", err, out)
	}
	runGit(t, dir, "config", "user.name", "gitobject test")
	runGit(t, dir, "config", "user.email", "gitobject@example.test")
	if err := os.WriteFile(filepath.Join(dir, "source.txt"), []byte("sha256\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "--", "source.txt")
	runGit(t, dir, "commit", "-q", "-m", "sha256")
	oid := runGit(t, dir, "rev-parse", "HEAD:source.txt")
	if len(oid) != 64 {
		t.Fatalf("SHA-256 object id length = %d", len(oid))
	}
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	request, _ := Exact(oid, Blob, true, 7)
	results, err := r.Read(t.Context(), []Request{request}, 7)
	if err != nil || string(results[0].Content) != "sha256\n" {
		t.Fatalf("SHA-256 read = %+v, %v", results, err)
	}
}

func TestReaderIgnoresInheritedGitEnvironment(t *testing.T) {
	f := newFixture(t, true)
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "hostile-git-dir"))
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(t.TempDir(), "hostile-objects"))
	t.Setenv("GIT_NAMESPACE", "hostile-namespace")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.repositoryformatversion")
	t.Setenv("GIT_CONFIG_VALUE_0", "999")

	r, err := Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	request, _ := Exact(f.blob, Blob, true, int64(len(f.body)))
	results, err := r.Read(t.Context(), []Request{request}, int64(len(f.body)))
	if err != nil || len(results) != 1 || !bytes.Equal(results[0].Content, f.body) {
		t.Fatalf("read under hostile inherited Git environment = %+v, %v", results, err)
	}
}

func TestReaderReturnsPositionPreservingMissingWrongTypeAndLimits(t *testing.T) {
	f := newFixture(t, true)
	r, err := Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	missingOID := strings.Repeat("f", len(f.blob))
	if missingOID == f.blob {
		missingOID = strings.Repeat("e", len(f.blob))
	}
	missing, _ := Exact(missingOID, Blob, true, 100)
	wrong, _ := Exact(f.commit, Blob, true, 100)
	large, _ := Exact(f.blob, Blob, true, int64(len(f.body)-1))
	results, err := r.Read(t.Context(), []Request{missing, wrong, large}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != Missing || results[1].Status != WrongType || results[2].Status != TooLarge || results[2].Bound != int64(len(f.body)-1) {
		t.Fatalf("statuses = %+v", results)
	}
	for _, result := range results {
		if result.Content != nil {
			t.Fatalf("non-available result returned content: %+v", result)
		}
	}

	a, _ := Exact(f.blob, Blob, true, int64(len(f.body)))
	b, _ := Exact(f.blob, Blob, true, int64(len(f.body)))
	_, err = r.Read(t.Context(), []Request{a, b}, int64(len(f.body)))
	var aggregate *AggregateLimitError
	if !errors.As(err, &aggregate) || aggregate.Bound != int64(len(f.body)) {
		t.Fatalf("aggregate error = %v", err)
	}
	// An aggregate refusal happens after metadata only and keeps the stream
	// aligned, so the same Reader remains usable.
	results, err = r.Read(t.Context(), []Request{a}, int64(len(f.body)))
	if err != nil || !bytes.Equal(results[0].Content, f.body) {
		t.Fatalf("reader after aggregate refusal = %+v, %v", results, err)
	}
}

func TestExactAndSpecValidationAndResolution(t *testing.T) {
	for _, oid := range []string{"", strings.Repeat("0", 40), strings.Repeat("g", 40), strings.Repeat("a", 39), strings.Repeat("a", 65)} {
		if _, err := Exact(oid, Blob, false, 0); err == nil {
			t.Errorf("Exact(%q) succeeded", oid)
		}
	}
	for _, spec := range []string{"", "HEAD\nother", "HEAD\x00other", strings.Repeat("x", MaxSpecBytes+1)} {
		if _, err := Spec(spec, Blob, false, 0); err == nil {
			t.Errorf("Spec(%q) succeeded", spec)
		}
	}
	if _, err := Spec("HEAD", Type("evil"), false, 0); err == nil {
		t.Fatal("unknown expected type succeeded")
	}
	if _, err := Spec("HEAD", Blob, true, -1); err == nil {
		t.Fatal("negative content limit succeeded")
	}

	f := newFixture(t, false)
	r, err := Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	request, _ := Spec("HEAD:source.txt", Blob, true, int64(len(f.body)))
	results, err := r.Read(t.Context(), []Request{request}, int64(len(f.body)))
	if err != nil || results[0].OID != f.blob || !bytes.Equal(results[0].Content, f.body) {
		t.Fatalf("spec result = %+v, %v", results, err)
	}
}

func TestReaderConcurrentCallersAreSerializedWithoutMixingResults(t *testing.T) {
	f := newFixture(t, true)
	r, err := Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	request, _ := Exact(f.blob, Blob, true, int64(len(f.body)))

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < cap(errs); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results, err := r.Read(t.Context(), []Request{request}, int64(len(f.body)))
			if err != nil {
				errs <- err
				return
			}
			if len(results) != 1 || !bytes.Equal(results[0].Content, f.body) {
				errs <- fmt.Errorf("mixed result: %+v", results)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestCloseIsIdempotentAndReadAfterCloseIsTyped(t *testing.T) {
	f := newFixture(t, false)
	r, err := Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	request, _ := Exact(f.blob, Blob, false, 0)
	if _, err := r.Read(t.Context(), []Request{request}, 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("read after close = %v", err)
	}
}

func TestCancellationWhileWaitingDoesNotPoisonActiveReader(t *testing.T) {
	f := newFixture(t, true)
	r, err := Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	request, _ := Exact(f.blob, Blob, true, int64(len(f.body)))

	// Hold the gate to model another active request without relying on timing
	// inside Git. A cancelled waiter must leave the Reader untouched.
	<-r.gate
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.Read(ctx, []Request{request}, int64(len(f.body))); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter = %v", err)
	}
	r.gate <- struct{}{}
	results, err := r.Read(t.Context(), []Request{request}, int64(len(f.body)))
	if err != nil || !bytes.Equal(results[0].Content, f.body) {
		t.Fatalf("reader was poisoned by waiting cancellation: %+v, %v", results, err)
	}
}

func TestOpenStartFailureDoesNotHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/nonexistent is the Unix start-failure control")
	}
	start := time.Now()
	if _, err := Open(t.TempDir(), WithBinary("/nonexistent/sigbound-git")); err == nil {
		t.Fatal("Open with missing binary succeeded")
	}
	if time.Since(start) > time.Second {
		t.Fatal("start failure took unexpectedly long")
	}
}

func TestActiveCancellationPoisonsAndBoundsAWedgedReader(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the wedged-child control is a POSIX shell helper")
	}
	helper := filepath.Join(t.TempDir(), "git-wedge")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nIFS= read -r line\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := Open(t.TempDir(), WithBinary(helper))
	if err != nil {
		t.Fatal(err)
	}
	oid := strings.Repeat("a", 40)
	request, _ := Exact(oid, Blob, false, 0)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	results, err := r.Read(ctx, []Request{request}, 0)
	if err == nil || results != nil || !errors.Is(err, ErrPoisoned) {
		t.Fatalf("wedged read = %+v, %v", results, err)
	}
	if elapsed := time.Since(start); elapsed > closeDelay+time.Second {
		t.Fatalf("cancellation took %s, bound is approximately %s", elapsed, closeDelay)
	}
	if _, err := r.Read(t.Context(), []Request{request}, 0); !errors.Is(err, ErrPoisoned) {
		t.Fatalf("reader after cancellation = %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkPackedReads(b *testing.B) {
	f := newFixture(b, true)
	for _, size := range []int{1, 100, MaxBatch} {
		b.Run(fmt.Sprintf("batch=%d/reused", size), func(b *testing.B) {
			requests := make([]Request, size)
			for i := range requests {
				requests[i], _ = Exact(f.blob, Blob, true, int64(len(f.body)))
			}
			r, err := Open(f.dir)
			if err != nil {
				b.Fatal(err)
			}
			defer r.Close()
			b.ReportAllocs()
			b.SetBytes(int64(len(f.body) * len(requests)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := r.Read(context.Background(), requests, int64(len(f.body)*len(requests))); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("batch=%d/process-per-object", size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(f.body) * size))
			for i := 0; i < b.N; i++ {
				for j := 0; j < size; j++ {
					cmd := exec.Command("git", "-C", f.dir, "cat-file", "blob", f.blob)
					cmd.Env = hermeticEnv()
					out, err := cmd.Output()
					if err != nil || !bytes.Equal(out, f.body) {
						b.Fatalf("cat-file blob = %d bytes, %v", len(out), err)
					}
				}
			}
		})
	}
}
