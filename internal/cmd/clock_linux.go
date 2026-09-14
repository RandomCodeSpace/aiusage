package cmd

import (
	"time"

	"golang.org/x/sys/unix"
)

// monotonicNow reads CLOCK_MONOTONIC, the clock systemd's *TimestampMonotonic
// unit properties are on, so an age computed against them is exact across a
// suspend. A package-level var so tests can pin the present.
var monotonicNow = func() (time.Duration, bool) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, false
	}
	return time.Duration(ts.Nano()), true
}
