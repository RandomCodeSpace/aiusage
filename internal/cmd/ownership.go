package cmd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/RandomCodeSpace/aiusage/internal/buildinfo"
	"github.com/RandomCodeSpace/aiusage/internal/config"
	"github.com/RandomCodeSpace/aiusage/internal/daemon"
	"github.com/RandomCodeSpace/aiusage/internal/service"
	"github.com/RandomCodeSpace/aiusage/store"
)

// collectorProcess reads immutable invocation evidence and the open writable
// ledger. A PID file alone cannot identify either a collector or its database.
// Unsupported or inaccessible process evidence stays unknown.
var collectorProcess = inspectCollectorProcess
var collectorProcRoot = "/proc"

func inspectCollectorProcess(ctx context.Context, pid int) (database string, collector bool) {
	if runtime.GOOS == "darwin" {
		return darwinCollectorProcess(ctx, pid)
	}
	if pid <= 0 || ctx.Err() != nil {
		return "", false
	}
	root := filepath.Join(collectorProcRoot, strconv.Itoa(pid))
	executable, err := os.Readlink(filepath.Join(root, "exe"))
	if err != nil {
		return "", false
	}
	executable = strings.TrimSuffix(executable, " (deleted)")
	if executable != selfPath() && filepath.Base(executable) != "aiusage" {
		return "", false
	}
	raw, err := os.ReadFile(filepath.Join(root, "cmdline"))
	if err != nil {
		return "", false
	}
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	run := false
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "run" {
			run = true
			continue
		}
		if arg == "--db" || arg == "--home" || arg == "--config" || arg == "--interval" {
			if i+1 >= len(args) {
				return "", false
			}
			i++
			continue
		}
		if strings.HasPrefix(arg, "--") {
			continue
		}
		return "", false
	}
	if !run {
		return "", false
	}
	// The writable descriptor proves the effective ledger even when the config
	// file or shell environment has changed since the process started.
	entries, err := os.ReadDir(filepath.Join(root, "fd"))
	if err != nil {
		return "", true
	}
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(root, "fd", entry.Name()))
		if err != nil || !filepath.IsAbs(target) {
			continue
		}
		info, err := os.ReadFile(filepath.Join(root, "fdinfo", entry.Name()))
		if err != nil {
			continue
		}
		writable := false
		for _, line := range strings.Split(string(info), "\n") {
			if value, ok := strings.CutPrefix(line, "flags:"); ok {
				mode, err := strconv.ParseUint(strings.TrimSpace(value), 8, 64)
				writable = err == nil && mode&3 == uint64(os.O_RDWR)
			}
		}
		if !writable {
			continue
		}
		// Do not probe lock files, pipes, or arbitrary device descriptors as SQLite.
		stat, err := os.Stat(target)
		if err != nil || !stat.Mode().IsRegular() {
			continue
		}
		f, err := os.Open(target)
		if err != nil {
			continue
		}
		var header [16]byte
		n, readErr := f.Read(header[:])
		_ = f.Close()
		if readErr != nil || n != len(header) || string(header[:]) != "SQLite format 3\x00" {
			continue
		}
		version, err := store.RecordedSchemaVersion(ctx, target)
		if err == nil && version > 0 {
			if database != "" && database != target {
				return "", true
			}
			database = target
		}
	}
	return database, true
}

func collectorOwner(ctx context.Context, m *service.Manager, pid int) (native bool, err error) {
	nativePID, known := m.CollectorPID(ctx)
	if pid <= 0 || !known {
		return false, fmt.Errorf("collector pid %d ownership is unknown; no lifecycle change was made", pid)
	}
	if nativePID == pid {
		return true, nil
	}
	if _, collector := collectorProcess(ctx, pid); !collector {
		return false, fmt.Errorf("collector pid %d is not a verified detached collector; no lifecycle change was made", pid)
	}
	return false, nil
}

// legacyCollector returns a proven same-ledger holder, or an error when the
// shared pre-isolation lock cannot safely be distinguished from this ledger.
func legacyCollector(ctx context.Context, cfg config.Config) (config.Config, bool, error) {
	legacy := cfg.LegacyPIDPath()
	if legacy == "" {
		return cfg, false, nil
	}
	previous := cfg
	previous.PIDPath = legacy
	running, pid := daemon.Status(previous)
	if !running {
		return cfg, false, nil
	}
	if _, err := collectorOwner(ctx, newSupervisor(), pid); err != nil {
		return cfg, false, fmt.Errorf("legacy holder %d on %s: %w", pid, legacy, err)
	}
	database, _ := collectorProcess(ctx, pid)
	if database == "" {
		return cfg, false, fmt.Errorf("legacy holder %d on %s has an unknown target ledger; collection was not started", pid, legacy)
	}
	if filepath.Clean(database) == filepath.Clean(cfg.DBPath) {
		return previous, true, nil
	}
	return cfg, false, nil
}

// acquireCollectorLock serializes the legacy recheck and namespaced acquisition.
// The caller keeps namespaced exclusivity through schema work and collection.
func acquireCollectorLock(ctx context.Context, cfg config.Config) (func(), error) {
	release, _, err := acquireCollectorStartupLock(ctx, cfg)
	return release, err
}

func acquireCollectorStartupLock(ctx context.Context, cfg config.Config) (release, started func(), err error) {
	releaseLegacy := func() {}
	if legacy := cfg.LegacyPIDPath(); legacy != "" {
		previous := cfg
		previous.PIDPath = legacy
		guard, err := daemon.AcquireCollectionLock(legacy, buildinfo.Identity())
		if err == nil {
			released := false
			releaseLegacy = func() {
				if !released {
					released = true
					guard()
				}
			}
		} else {
			_, same, probeErr := legacyCollector(ctx, cfg)
			if probeErr != nil {
				return nil, nil, probeErr
			}
			if same {
				return nil, nil, fmt.Errorf("legacy collector already collects %s under %s: %w", cfg.DBPath, legacy, syscall.EWOULDBLOCK)
			}
			// Failure without a current owner is not permission to ignore the guard.
			if running, _ := daemon.Status(previous); !running {
				return nil, nil, err
			}
		}
	}
	releaseCollector, err := daemon.AcquireCollectionLock(cfg.PIDPath, buildinfo.Identity())
	if err != nil {
		releaseLegacy()
		return nil, nil, err
	}
	return func() { releaseCollector(); releaseLegacy() }, releaseLegacy, nil
}

// The Darwin file supplies the kernel's argument bytes. Other platforms retain
// this error so tests can inject the native format without calling a host tool.
var readDarwinProcessArgs = func(int) ([]byte, error) { return nil, errors.New("darwin process arguments unavailable") }

func darwinCollectorProcess(ctx context.Context, pid int) (string, bool) {
	if pid <= 0 || ctx.Err() != nil {
		return "", false
	}
	raw, err := readDarwinProcessArgs(pid)
	if err != nil || len(raw) < 4 {
		return "", false
	}
	argc := int(binary.NativeEndian.Uint32(raw[:4]))
	executable, remaining, ok := bytes.Cut(raw[4:], []byte{0})
	if !ok || argc < 1 || argc > len(remaining) {
		return "", false
	}
	if string(executable) != selfPath() && filepath.Base(string(executable)) != "aiusage" {
		return "", false
	}
	remaining = bytes.TrimLeft(remaining, "\x00")
	args := make([]string, 0, argc)
	for i := 0; i < argc; i++ {
		arg, tail, ok := bytes.Cut(remaining, []byte{0})
		if !ok {
			return "", false
		}
		args = append(args, string(arg))
		remaining = tail
	}
	args = args[1:]
	run := false
	database := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "run" && !run {
			run = true
			continue
		}
		if arg == "--no-daemon" {
			continue
		}
		name, value, inline := strings.Cut(arg, "=")
		switch name {
		case "--db", "--config", "--home", "--interval":
			if !inline {
				i++
				if i >= len(args) {
					return "", false
				}
				value = args[i]
			}
			if value == "" || strings.HasPrefix(value, "--") {
				return "", false
			}
			if name == "--db" {
				if !filepath.IsAbs(value) {
					return "", false
				}
				database = filepath.Clean(value)
			}
		default:
			return "", false
		}
	}
	return database, run
}
