//go:build !unix

package main

// statfsFree is the non-unix fallback: Go's syscall package has no Statfs on
// windows/js/wasip1/plan9, and reimplementing each platform's own disk-usage
// API (e.g. GetDiskFreeSpaceEx on windows) isn't worth it for a preflight
// that's explicitly best-effort. Reporting "unknown" here is exactly what
// diskPreflight/diskInfoLine already treat as "skip the check, never fail
// the run" — see their doc comments.
func statfsFree(path string) (bytesFree uint64, ok bool) {
	return 0, false
}
