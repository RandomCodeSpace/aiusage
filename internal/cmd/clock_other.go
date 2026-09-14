//go:build !linux

package cmd

import "time"

// monotonicNow is unknown off Linux: the only consumer compares against
// systemd's monotonic timestamps, and there is no systemd to compare with.
var monotonicNow = func() (time.Duration, bool) { return 0, false }
