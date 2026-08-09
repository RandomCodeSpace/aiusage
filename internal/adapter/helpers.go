package adapter

import (
	"os"
	"path/filepath"
)

// Shared filesystem/number helpers used by the adapters, so each one does not
// carry its own copy.

// NonNeg clamps a possibly-negative counter to zero.
func NonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// IsDir reports whether path exists and is a directory.
func IsDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// FileStem returns the file name without directory or extension.
func FileStem(path string) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	return base
}
