package daemon

import (
	"os"
)

// MaxLogBytes is the restart-time cap for the collector's file log. Collection
// errors repeat every cycle, so a detached spawn or LaunchAgent activation
// rotates an oversized log before the replacement process opens it.
const MaxLogBytes int64 = 10 << 20

// RotateLog renames an oversized log to <path>.old, replacing the previous
// rotation. It is best-effort: logging must not prevent collection from
// starting when a stat or rename is refused.
func RotateLog(path string) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() <= MaxLogBytes {
		return
	}
	_ = os.Rename(path, path+".old")
}
