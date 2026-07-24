package gitx

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// requirePOSIXShell skips a test that depends on a POSIX shell — here, the
// #!/bin/sh fake-git script fakeGitBinary writes. Windows CI ships no such
// shell. See issue #94.
func requirePOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a #!/bin/sh fake git binary; needs a POSIX shell, unix-only (issue #94)")
	}
}

// requireUnixExecBit skips a test that asserts git's 100755 executable mode.
// Git on Windows does not track the filesystem exec bit (core.filemode is
// effectively false there), so a chmod +x cannot produce a 100755 tree entry.
// The code under test is platform-agnostic — it preserves whatever mode git
// recorded — only this fixture's premise is unix-only. See issue #94.
func requireUnixExecBit(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("asserts a 100755 exec-bit tree entry, which git on Windows does not track from a chmod (issue #94)")
	}
}

func TestParseGitVersion(t *testing.T) {
	cases := []struct {
		name      string
		out       string
		wantMajor int
		wantMinor int
		wantErr   bool
	}{
		{"exact floor", "git version 2.38.0", 2, 38, false},
		{"just below floor", "git version 2.37.9", 2, 37, false},
		{"apple suffix", "git version 2.39.3 (Apple Git-146)", 2, 39, false},
		{"windows suffix", "git version 2.43.0.windows.1", 2, 43, false},
		{"trailing newline", "git version 2.42.0\n", 2, 42, false},
		{"empty", "", 0, 0, true},
		{"garbage", "not a git binary at all", 0, 0, true},
		{"no minor", "git version 2", 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			major, minor, err := ParseGitVersion(tc.out)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseGitVersion(%q) = %d.%d, nil; want an error", tc.out, major, minor)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGitVersion(%q) unexpected error: %v", tc.out, err)
			}
			if major != tc.wantMajor || minor != tc.wantMinor {
				t.Fatalf("ParseGitVersion(%q) = %d.%d, want %d.%d", tc.out, major, minor, tc.wantMajor, tc.wantMinor)
			}
		})
	}
}

// TestCheckMinVersionBoundary exercises the >= 2.38 boundary end to end
// through CheckMinVersion using a fake "git" script so the test doesn't
// depend on the real git installed in CI.
func TestCheckMinVersionBoundary(t *testing.T) {
	cases := []struct {
		name    string
		version string
		wantOK  bool
	}{
		{"2.37 fails", "2.37.0", false},
		{"2.38 ok", "2.38.0", true},
		{"2.39.3 ok", "2.39.3", true},
		{"3.0 ok", "3.0.0", true},
		{"1.9 fails", "1.9.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeGitBinary(t, "git version "+tc.version+" (Apple Git-999)")
			err := CheckMinVersion(context.Background(), bin)
			if tc.wantOK && err != nil {
				t.Fatalf("CheckMinVersion with %q: unexpected error: %v", tc.version, err)
			}
			if !tc.wantOK {
				if err == nil {
					t.Fatalf("CheckMinVersion with %q: want error, got nil", tc.version)
				}
				if !strings.Contains(err.Error(), "requires git >= 2.38") {
					t.Fatalf("error should name the requirement: %v", err)
				}
				if !strings.Contains(err.Error(), "sig doctor") {
					t.Fatalf("error should point at `sig doctor`: %v", err)
				}
			}
		})
	}
}

func TestCheckMinVersionMissingBinary(t *testing.T) {
	err := CheckMinVersion(context.Background(), "sig-doctor-definitely-not-a-real-binary")
	if err == nil {
		t.Fatal("want an error for a nonexistent git binary")
	}
	if !strings.Contains(err.Error(), "sig doctor") {
		t.Fatalf("error should point at `sig doctor`: %v", err)
	}
}

// fakeGitBinary writes a tiny shell script that prints out for `<bin>
// version` and returns its path, so version-boundary tests don't depend on
// whatever git happens to be installed in CI.
func fakeGitBinary(t *testing.T, out string) string {
	t.Helper()
	requirePOSIXShell(t)
	path := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\necho '" + out + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
