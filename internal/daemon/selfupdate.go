package daemon

import (
	"errors"
	"os"
	"time"
)

// ErrBinaryReplaced is returned by Run when the executable it was started
// from has been overwritten on disk — an upgrade landed while the daemon was
// running. It is a signal, not a fault: the caller is expected to release its
// resources and exec the new binary (cmd.newRunCmd does).
//
// Without it the collector keeps running the old code indefinitely. The CLI's
// version check (cmd.ensureDaemon) only fires when someone runs a command, so a
// `go install` followed by nothing at all would leave the superseded build
// collecting until the next invocation — which, for a background daemon whose
// whole point is to run unattended, can be days.
var ErrBinaryReplaced = errors.New("daemon binary replaced on disk")

// execStamp identifies the bytes behind the daemon's own executable. Size and
// modification time are enough: an install rewrites the file, and no two builds
// land at the same nanosecond with the same length.
type execStamp struct {
	size  int64
	mtime time.Time
}

// same reports whether two stamps describe the same file contents.
func (s execStamp) same(o execStamp) bool {
	return s.size == o.size && s.mtime.Equal(o.mtime)
}

// stampExecutable stats path and reports whether it could be stamped at all.
//
// A path that cannot be stat'd is deliberately NOT a replacement: `go run`
// deletes its temporary binary the moment the parent exits, an installer may
// unlink before renaming, and a daemon that restarted itself over a file that
// is simply gone would be restarting into nothing. Only an executable that is
// present AND different counts, so the false negative (a missed upgrade, caught
// by the CLI's own version check on the next command) is always preferred to
// the false positive.
func stampExecutable(path string) (execStamp, bool) {
	if path == "" {
		return execStamp{}, false
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return execStamp{}, false
	}
	return execStamp{size: fi.Size(), mtime: fi.ModTime()}, true
}
