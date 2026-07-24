//go:build unix

package main

import "syscall"

// statfsFree reports the bytes free on the filesystem containing path,
// available to an UNPRIVILEGED user (Bavail, not Bfree — Bfree includes
// space reserved for root, which this process can't actually spend), via
// syscall.Statfs. ok is false only when the syscall itself fails (path
// doesn't exist, permission denied, etc.) — see diskPreflight/diskInfoLine
// for how that's treated (skip the check, never fail on a reading it
// couldn't take).
func statfsFree(path string) (bytesFree uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}
