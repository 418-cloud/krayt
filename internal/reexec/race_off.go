//go:build !race

package reexec

// RaceEnabled mirrors the stdlib-internal internal/race.Enabled, which is not importable from
// here. It is what makes FastExit inert on the builds that have no teardown sleep to skip.
const RaceEnabled = false
