package main

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
)

// Version is the sigbound release version. It defaults to the version being
// developed toward and can be overridden at build time:
//
//	go build -ldflags "-X main.Version=0.1.0" ./cmd/sig
var Version = "0.3.0"

// runVersion prints the version and, when the binary was built from a git
// checkout, the commit and build date recorded by the Go toolchain via
// runtime/debug.ReadBuildInfo. Nothing here fails: missing build info just
// means those lines are omitted.
func runVersion(w io.Writer) {
	fmt.Fprintf(w, "sig %s\n", Version)

	commit, date, dirty := vcsInfo()
	if commit != "" {
		suffix := ""
		if dirty {
			suffix = " (dirty)"
		}
		fmt.Fprintf(w, "commit: %s%s\n", commit, suffix)
	}
	if date != "" {
		fmt.Fprintf(w, "built:  %s\n", date)
	}
	fmt.Fprintf(w, "go:     %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// vcsInfo reads the vcs.* build settings the Go toolchain stamps into a binary
// built from a git working tree. Returns empty strings when unavailable (e.g.
// `go run`, or a build with -buildvcs=false).
func vcsInfo() (commit, date string, dirty bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", "", false
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			commit = s.Value
			if len(commit) > 12 {
				commit = commit[:12]
			}
		case "vcs.time":
			date = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	return commit, date, dirty
}
