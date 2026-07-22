package gitx

// This file is the version-capability gate: sigbound's engine hard-depends on
// `git merge-tree --write-tree -z --name-only --merge-base=` (see MergeTree)
// and the index plumbing OverlayTrees drives, both of which require git >=
// 2.38. Without a guard, an older git fails deep inside a run with a bare
// "merge-tree exit N" — this file turns that into an actionable error up
// front. CheckMinVersion is the CHEAP check (parse `git version`, compare
// numbers); it does not prove the plumbing actually works — see cmd/sig's
// `sig doctor` for the live probe that does.

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// minGitMajor/minGitMinor is the oldest git version sigbound supports.
const (
	minGitMajor = 2
	minGitMinor = 38
)

// versionFieldRe extracts the leading X.Y from `git version` output, e.g.
// "git version 2.39.3 (Apple Git-146)" -> "2.39", "git version 2.43.0.windows.1"
// -> "2.43". It takes the FIRST "\d+\.\d+" in the string, which is always the
// version number itself (nothing earlier in git's output looks like one).
var versionFieldRe = regexp.MustCompile(`(\d+)\.(\d+)`)

// ParseGitVersion extracts the major.minor version from `git version` output.
// It is a pure function of untrusted external command output and must never
// panic on malformed input. Fuzzed by FuzzParseGitVersion.
func ParseGitVersion(out string) (major, minor int, err error) {
	m := versionFieldRe.FindStringSubmatch(out)
	if m == nil {
		return 0, 0, fmt.Errorf("no version number found in %q", strings.TrimSpace(out))
	}
	major, majErr := strconv.Atoi(m[1])
	minor, minErr := strconv.Atoi(m[2])
	if majErr != nil || minErr != nil {
		// The regex only matches digit runs, but a run long enough overflows
		// int (strconv.Atoi errors rather than wrapping) — reachable from
		// hostile/malformed output, so this must be a clean rejection, not a
		// panic. See FuzzParseGitVersion.
		return 0, 0, fmt.Errorf("unparseable version number in %q", strings.TrimSpace(out))
	}
	return major, minor, nil
}

// GitVersion runs `<bin> version` and returns the raw output plus its parsed
// major.minor. bin is not tied to any repository directory (git version needs
// none), so this is a package-level function rather than a *Git method.
func GitVersion(ctx context.Context, bin string) (raw string, major, minor int, err error) {
	out, err := exec.CommandContext(ctx, bin, "version").Output()
	if err != nil {
		return "", 0, 0, fmt.Errorf("%s version: %w", bin, err)
	}
	raw = strings.TrimSpace(string(out))
	major, minor, err = ParseGitVersion(raw)
	if err != nil {
		return raw, 0, 0, fmt.Errorf("%s version: %w", bin, err)
	}
	return raw, major, minor, nil
}

// CheckMinVersion is the cheap preflight: it verifies bin runs and reports a
// version >= 2.38. It does NOT exercise merge-tree or the overlay plumbing
// (that's the live probe in `sig doctor`) — this is deliberately fast enough
// to run on every `sig run` / `sig integrate` invocation. The error is always
// actionable and names `sig doctor` for the full picture.
func CheckMinVersion(ctx context.Context, bin string) error {
	raw, major, minor, err := GitVersion(ctx, bin)
	if err != nil {
		return fmt.Errorf("%w; sigbound requires git >= %d.%d; run `sig doctor` for details", err, minGitMajor, minGitMinor)
	}
	if major < minGitMajor || (major == minGitMajor && minor < minGitMinor) {
		return fmt.Errorf("%s reports version %q (%d.%d); sigbound requires git >= %d.%d; run `sig doctor` for details",
			bin, raw, major, minor, minGitMajor, minGitMinor)
	}
	return nil
}
