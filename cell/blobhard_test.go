package cell

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/surya-koritala/sigbound/internal/gitx"
)

// TestDaemonEqualsSpawnHardBlobs extends the byte-equality proof to the hard
// shapes: blobs far larger than bufio's 4KB/64KB buffers, content that resembles
// batch headers, content full of embedded newlines and NULs, and a blob whose
// length lands exactly on a buffer boundary. The daemon (persistent cat-file
// --batch, read via bufio + io.ReadFull) must return bytes identical to the
// per-call spawn for ALL of them.
func TestDaemonEqualsSpawnHardBlobs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := gitx.New(dir)
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}

	// A 256 KiB body mixing NULs, newlines, and lines that LOOK like batch
	// headers ("<40 hex> blob <n>") — the exact bytes a desync bug would
	// misframe. Well past bufio.NewReader's default 4096 and 64 KiB buffers.
	var big bytes.Buffer
	for big.Len() < 256*1024 {
		big.WriteString("0000000000000000000000000000000000000000 blob 12345\n")
		big.Write([]byte{0, 1, 2, 0, 255, 10, 0})
		big.WriteString("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef missing\n")
	}

	files := map[string]string{
		"big.dat":           big.String(),                   // >64KB, header-like + NUL + LF
		"looks_like_header": "abc123 blob 5\nhello world\n", // whole body resembles a record
		"embedded_nl.txt":   "a\nb\nc\n\n\nd\n",             // many newlines, incl. blank lines
		"trailing_nul.dat":  string([]byte{9, 9, 0}),        // ends in NUL (git adds its own LF after)
		"empty.txt":         "",
		"exact_4096.dat":    string(bytes.Repeat([]byte("x"), 4096)), // lands on bufio's default boundary
		"exact_4095.dat":    string(bytes.Repeat([]byte("y"), 4095)),
		"exact_4097.dat":    string(bytes.Repeat([]byte("z"), 4097)),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base, err := g.CommitAll(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}

	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	specs := make([]string, 0, len(files)+3)
	for name := range files {
		specs = append(specs, base+":"+name)
	}
	// Salt in a missing path and a non-blob (tree) spec, interleaved with the big
	// blobs, to prove framing survives absent records mid-batch too.
	specs = append(specs, base+":does-not-exist", base+"^{tree}", base)

	viaSpawn, err := c.Git().BlobsBatch(ctx, specs)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	viaDaemon, err := c.BlobsBatch(ctx, specs)
	if err != nil {
		t.Fatalf("daemon: %v", err)
	}

	// Byte-for-byte against the on-disk truth for every present file.
	for name, body := range files {
		got := viaDaemon[base+":"+name]
		if got != body {
			t.Fatalf("daemon %s: %d bytes != want %d bytes (first-diff check)", name, len(got), len(body))
		}
	}
	// Daemon and spawn must be map-for-map identical, including absent specs.
	if len(viaDaemon) != len(viaSpawn) {
		t.Fatalf("map sizes differ: daemon=%d spawn=%d", len(viaDaemon), len(viaSpawn))
	}
	for k, v := range viaSpawn {
		if viaDaemon[k] != v {
			t.Fatalf("daemon[%q] (%d bytes) != spawn (%d bytes)", k, len(viaDaemon[k]), len(v))
		}
	}
	// The missing and tree specs must be absent from BOTH.
	for _, spec := range []string{base + ":does-not-exist", base + "^{tree}", base} {
		if _, ok := viaDaemon[spec]; ok {
			t.Fatalf("non-blob/missing spec %q must be absent", spec)
		}
	}
}
