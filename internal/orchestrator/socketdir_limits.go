package orchestrator

import "path/filepath"

// maxUnixSocketPath is the longest path bind(2) will accept for a unix socket, taken as the
// SHORTEST limit krayt has to run under rather than the local one: macOS's sockaddr_un.sun_path
// is 104 bytes including the NUL, so 103 characters. Linux allows 107, and Windows' AF_UNIX
// (afunix.h) uses the same 108-byte sun_path POSIX does. Using the smallest number everywhere
// means a run that works on one platform works on all of them, and costs nothing — no krayt
// socket is anywhere near any of the limits by itself.
//
// Exceeding it does not fail politely. bind returns EINVAL, which surfaces as the thoroughly
// unhelpful "bind: invalid argument" with no mention of length at all — on Windows as much as on
// Linux or macOS.
const maxUnixSocketPath = 103

// runSocketNames is every basename bound inside a run's socket directory. The budget check below
// has to clear the LONGEST of them, not just the one being bound at that moment, or a run would
// bind ask.sock successfully and then fail three statements later on control.sock.
var runSocketNames = []string{"ask.sock", "control.sock"}

// socketDirFits reports whether every socket krayt binds in dir stays under the limit.
func socketDirFits(dir string) bool { return longestSocketPathLen(dir) <= maxUnixSocketPath }

func longestSocketPathLen(dir string) int {
	longest := 0
	for _, n := range runSocketNames {
		if l := len(filepath.Join(dir, n)); l > longest {
			longest = l
		}
	}
	return longest
}
