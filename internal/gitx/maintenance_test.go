package gitx

import (
	"context"
	"testing"
)

// TestInitDisablesBackgroundMaintenance pins both switches on a repo Init made.
//
// This is a TEARDOWN correctness property, not a tuning preference. Both
// mechanisms daemonize and outlive the command that triggered them, so a
// process can still be writing into .git after the foreground git command has
// returned — which is what makes `RemoveAll` fail with "directory not empty"
// and turns a passing test red in cleanup (issue #166).
//
// They are separate switches, each enabled by default, and gc.auto does NOT
// cover maintenance.auto. Asserting only the one that was already set would be
// asserting nothing: it is the second that was missing.
func TestInitDisablesBackgroundMaintenance(t *testing.T) {
	ctx := context.Background()
	g := New(t.TempDir())
	if err := g.Init(ctx); err != nil {
		t.Fatal(err)
	}

	for _, want := range []struct{ key, value string }{
		{"gc.auto", "0"},
		{"maintenance.auto", "false"},
	} {
		got, err := g.run(ctx, "config", "--get", want.key)
		if err != nil {
			t.Fatalf("%s is unset on a repo Init made: git may still run it in the background after a command returns (%v)", want.key, err)
		}
		if trimmed := trimLine(got); trimmed != want.value {
			t.Fatalf("%s=%q, want %q", want.key, trimmed, want.value)
		}
	}
}

// trimLine drops the trailing newline git leaves on `config --get`.
func trimLine(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
