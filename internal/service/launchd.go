package service

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/daemon"
)

const launchdStamp = "<!-- " + stampToken + " -->"

// DefaultLaunchAgentDir returns the logged-in user's third-party LaunchAgent
// directory. A rootless agent belongs here, never in /Library/LaunchDaemons.
func DefaultLaunchAgentDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents")
}

func renderLaunchAgent(o Options) (string, error) {
	if o.WorkingDir == "" {
		return "", fmt.Errorf("render LaunchAgent: user home is empty")
	}
	if o.LogPath == "" {
		return "", fmt.Errorf("render LaunchAgent: daemon log path is empty")
	}

	args := make([]string, 0, len(o.Args)+2)
	args = append(args, o.Exec, "run")
	args = append(args, o.Args...)

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(launchdStamp + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	plistString(&b, "Label", CollectLabel)
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, arg := range args {
		b.WriteString("    <string>" + xmlText(arg) + "</string>\n")
	}
	b.WriteString("  </array>\n")
	plistBool(&b, "RunAtLoad", true)
	plistBool(&b, "KeepAlive", true)
	plistString(&b, "ProcessType", "Background")
	plistInteger(&b, "ThrottleInterval", 10)
	plistString(&b, "WorkingDirectory", o.WorkingDir)
	plistString(&b, "StandardOutPath", o.LogPath)
	plistString(&b, "StandardErrorPath", o.LogPath)
	b.WriteString("</dict>\n</plist>\n")
	return b.String(), nil
}

func plistString(b *strings.Builder, key, value string) {
	b.WriteString("  <key>" + key + "</key>\n")
	b.WriteString("  <string>" + xmlText(value) + "</string>\n")
}

func plistBool(b *strings.Builder, key string, value bool) {
	b.WriteString("  <key>" + key + "</key>\n")
	if value {
		b.WriteString("  <true/>\n")
		return
	}
	b.WriteString("  <false/>\n")
}

func plistInteger(b *strings.Builder, key string, value int) {
	b.WriteString("  <key>" + key + "</key>\n")
	b.WriteString("  <integer>" + strconv.Itoa(value) + "</integer>\n")
}

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func launchdLogPath(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	decoder := xml.NewDecoder(f)
	pendingKey := ""
	for {
		token, err := decoder.Token()
		if err != nil {
			return ""
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "key":
			if err := decoder.DecodeElement(&pendingKey, &start); err != nil {
				return ""
			}
		case "string":
			var value string
			if err := decoder.DecodeElement(&value, &start); err != nil {
				return ""
			}
			if pendingKey == "StandardOutPath" {
				return value
			}
			pendingKey = ""
		}
	}
}

func (m *Manager) detectLaunchd(ctx context.Context) bool {
	if m.Run == nil {
		if _, err := exec.LookPath("launchctl"); err != nil {
			return false
		}
	}
	_, err := m.launchctl(ctx, "print", m.launchDomain())
	return err == nil
}

type launchActivation struct {
	loadedHere  bool
	enabledHere bool
}

func (m *Manager) installLaunchd(ctx context.Context, o Options) (Result, error) {
	var r Result
	if o.Exec == "" {
		return r, fmt.Errorf("install: the aiusage binary has no resolvable path")
	}
	body, err := renderLaunchAgent(o)
	if err != nil {
		return r, err
	}

	dir := m.unitDir()
	path := filepath.Join(dir, CollectPlist)
	existed := fileExists(path)
	var original []byte
	originalMode := os.FileMode(unitFileMode)
	if existed && o.Force {
		original, err = os.ReadFile(path)
		if err != nil {
			return r, fmt.Errorf("read %s before replacement: %w", path, err)
		}
		if fi, statErr := os.Stat(path); statErr == nil {
			originalMode = fi.Mode().Perm()
		}
	}

	wasLoaded, wasRunning, stateKnown := m.launchdState(ctx)
	if !stateKnown {
		return r, fmt.Errorf("inspect %s before installation: launchd did not provide a usable answer", CollectLabel)
	}
	wasDisabled, disabledKnown := m.launchdDisabled(ctx)
	if !disabledKnown {
		return r, fmt.Errorf("inspect %s persistence before installation: launchd did not provide a usable answer", CollectLabel)
	}

	dirExisted := fileExists(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return r, fmt.Errorf("create LaunchAgent directory %s: %w", dir, err)
	}
	r.addf("LaunchAgent directory: %s", dir)

	logDir := filepath.Dir(o.LogPath)
	logDirCreated := logDir != "" && logDir != "." && !fileExists(logDir)
	if logDirCreated {
		if err := os.MkdirAll(logDir, 0o700); err != nil {
			if !dirExisted {
				_ = os.Remove(dir)
			}
			return r, fmt.Errorf("create daemon log directory %s: %w", logDir, err)
		}
	}

	wrote := false
	rewritten := false
	if existed && !o.Force {
		r.addf("%s already present, left as it is", CollectPlist)
	} else {
		tmp, prepErr := m.prepareLaunchAgent(ctx, dir, body)
		if prepErr != nil {
			m.removeCreatedDirs(logDir, logDirCreated, dir, dirExisted)
			return r, prepErr
		}
		defer os.Remove(tmp)

		if existed && wasLoaded {
			if _, err := m.launchctl(ctx, "bootout", m.launchTarget()); err != nil {
				m.removeCreatedDirs(logDir, logDirCreated, dir, dirExisted)
				return r, fmt.Errorf("boot out %s before replacement: %w", CollectLabel, err)
			}
		}
		if err := os.Rename(tmp, path); err != nil {
			if existed && wasLoaded {
				_ = m.restoreLaunchdJob(ctx, path, wasRunning)
			}
			m.removeCreatedDirs(logDir, logDirCreated, dir, dirExisted)
			return r, fmt.Errorf("install %s: %w", path, err)
		}
		wrote = true
		rewritten = existed
		if rewritten {
			r.change("rewrote %s", path)
		} else {
			r.change("wrote %s", path)
		}
	}

	if !wasRunning || rewritten {
		logPath := o.LogPath
		if existed && !rewritten {
			logPath = launchdLogPath(path)
		}
		daemon.RotateLog(logPath)
	}
	attempt, err := m.activateLaunchd(ctx, path, wasLoaded && !rewritten, wasRunning && !rewritten, wasDisabled, &r)
	if err != nil {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.timeout())
		defer cancel()
		rollbackErr := m.rollbackLaunchd(rollbackCtx, path, wrote, existed, original, originalMode,
			wasLoaded, wasRunning, attempt, logDir, logDirCreated, dir, dirExisted)
		if rollbackErr != nil {
			return r, fmt.Errorf("%w; rollback failed: %v", err, rollbackErr)
		}
		return r, err
	}

	r.Collecting = true
	r.addf("persistence: %s (starts at GUI login after reboot; stops at logout)", PersistenceGUILogin)
	return r, nil
}

func (m *Manager) prepareLaunchAgent(ctx context.Context, dir, body string) (string, error) {
	f, err := os.CreateTemp(dir, ".aiusage-collect-*.plist")
	if err != nil {
		return "", fmt.Errorf("prepare LaunchAgent: %w", err)
	}
	path := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if err := f.Chmod(unitFileMode); err != nil {
		return "", fmt.Errorf("set LaunchAgent permissions: %w", err)
	}
	if _, err := f.WriteString(body); err != nil {
		return "", fmt.Errorf("write temporary LaunchAgent: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close temporary LaunchAgent: %w", err)
	}
	if _, err := m.command(ctx, "plutil", "-lint", path); err != nil {
		return "", fmt.Errorf("validate LaunchAgent with plutil: %w", err)
	}
	ok = true
	return path, nil
}

func (m *Manager) activateLaunchd(ctx context.Context, path string, loaded, running, wasDisabled bool, r *Result) (launchActivation, error) {
	var attempt launchActivation
	if wasDisabled || !loaded {
		if _, err := m.launchctl(ctx, "enable", m.launchTarget()); err != nil {
			return attempt, fmt.Errorf("enable %s: %w", CollectLabel, err)
		}
		if wasDisabled {
			attempt.enabledHere = true
			r.change("enabled %s", CollectLabel)
		}
	}

	if !loaded {
		if _, err := m.launchctl(ctx, "bootstrap", m.launchDomain(), path); err != nil {
			return attempt, fmt.Errorf("load %s: %w", CollectLabel, err)
		}
		attempt.loadedHere = true
		r.change("loaded %s", CollectLabel)
	} else {
		r.addf("%s already loaded", CollectLabel)
	}

	if running {
		r.addf("%s already running", CollectLabel)
		return attempt, nil
	}
	if _, err := m.launchctl(ctx, "kickstart", m.launchTarget()); err != nil {
		return attempt, fmt.Errorf("start %s: %w", CollectLabel, err)
	}
	if !m.awaitLaunchdRunning(ctx) {
		return attempt, fmt.Errorf("start %s: launchd did not confirm a running job", CollectLabel)
	}
	r.change("started %s", CollectLabel)
	return attempt, nil
}

func (m *Manager) rollbackLaunchd(ctx context.Context, path string, wrote, existed bool, original []byte,
	originalMode os.FileMode, wasLoaded, wasRunning bool, attempt launchActivation,
	logDir string, logDirCreated bool, dir string, dirExisted bool,
) error {
	var failures []string
	if attempt.loadedHere {
		if _, err := m.launchctl(ctx, "bootout", m.launchTarget()); err != nil {
			failures = append(failures, "boot out attempted job: "+err.Error())
		}
	}
	if wrote {
		if existed {
			if err := writeAtomicFile(path, original, originalMode); err != nil {
				failures = append(failures, "restore prior plist: "+err.Error())
			}
		} else if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			failures = append(failures, "remove new plist: "+err.Error())
		}
	}
	if wasLoaded && wrote {
		if err := m.restoreLaunchdJob(ctx, path, wasRunning); err != nil {
			failures = append(failures, "restore prior job: "+err.Error())
		}
	}
	if attempt.enabledHere {
		if _, err := m.launchctl(ctx, "disable", m.launchTarget()); err != nil {
			failures = append(failures, "restore disabled state: "+err.Error())
		}
	}
	m.removeCreatedDirs(logDir, logDirCreated, dir, dirExisted)
	if len(failures) > 0 {
		return fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return nil
}

func (m *Manager) restoreLaunchdJob(ctx context.Context, path string, running bool) error {
	if _, err := m.launchctl(ctx, "bootstrap", m.launchDomain(), path); err != nil {
		return err
	}
	if running {
		_, err := m.launchctl(ctx, "kickstart", m.launchTarget())
		return err
	}
	return nil
}

func (m *Manager) removeCreatedDirs(logDir string, logDirCreated bool, dir string, dirExisted bool) {
	if logDirCreated {
		_ = os.Remove(logDir)
	}
	if !dirExisted {
		_ = os.Remove(dir)
	}
}

func (m *Manager) restartLaunchd(ctx context.Context) (Result, error) {
	var r Result
	path := filepath.Join(m.unitDir(), CollectPlist)
	if !fileExists(path) {
		return r, nil
	}
	loaded, running, known := m.launchdState(ctx)
	if !known {
		return r, fmt.Errorf("restart %s: launchd did not provide a usable answer", CollectLabel)
	}
	if !loaded || !running {
		return r, nil
	}
	daemon.RotateLog(launchdLogPath(path))
	if _, err := m.launchctl(ctx, "bootout", m.launchTarget()); err != nil {
		return r, fmt.Errorf("restart %s: stop current job: %w", CollectLabel, err)
	}
	if _, err := m.launchctl(ctx, "bootstrap", m.launchDomain(), path); err != nil {
		return r, fmt.Errorf("restart %s: reload job: %w", CollectLabel, err)
	}
	if _, err := m.launchctl(ctx, "kickstart", m.launchTarget()); err != nil {
		return r, fmt.Errorf("restart %s: %w", CollectLabel, err)
	}
	if !m.awaitLaunchdRunning(ctx) {
		return r, fmt.Errorf("restart %s: launchd did not confirm a running job", CollectLabel)
	}
	r.change("restarted %s", CollectLabel)
	r.Collecting = true
	return r, nil
}

func (m *Manager) stopLaunchdCollection(ctx context.Context) (bool, error) {
	if !fileExists(filepath.Join(m.unitDir(), CollectPlist)) {
		return false, nil
	}
	loaded, running, known := m.launchdState(ctx)
	if !known {
		return false, fmt.Errorf("read %s state: launchd did not provide a usable answer", CollectLabel)
	}
	if !loaded || !running {
		return false, nil
	}
	if _, err := m.launchctl(ctx, "bootout", m.launchTarget()); err != nil {
		return false, fmt.Errorf("stop %s: %w", CollectLabel, err)
	}
	return true, nil
}

func (m *Manager) startLaunchdCollection(ctx context.Context) error {
	path := filepath.Join(m.unitDir(), CollectPlist)
	if !fileExists(path) {
		return fmt.Errorf("start %s: LaunchAgent is not installed", CollectLabel)
	}
	loaded, running, known := m.launchdState(ctx)
	if !known {
		return fmt.Errorf("read %s state: launchd did not provide a usable answer", CollectLabel)
	}
	if running {
		return nil
	}
	daemon.RotateLog(launchdLogPath(path))
	if !loaded {
		if _, err := m.launchctl(ctx, "bootstrap", m.launchDomain(), path); err != nil {
			return fmt.Errorf("load %s: %w", CollectLabel, err)
		}
	}
	if _, err := m.launchctl(ctx, "kickstart", m.launchTarget()); err != nil {
		return fmt.Errorf("start %s: %w", CollectLabel, err)
	}
	if !m.awaitLaunchdRunning(ctx) {
		return fmt.Errorf("start %s: launchd did not confirm a running job", CollectLabel)
	}
	return nil
}

// launchdStartGrace bounds how long a job is given after kickstart to be
// reported as running. kickstart returns once launchd has accepted the
// request, and `launchctl print` read in the same instant can still show the
// job in its previous state — observed on a macOS 15 runner, where a single
// immediate read failed a release that the same code had passed the day
// before. Polling closes the gap; the bound keeps a job that never comes up
// from stalling the caller past its own deadline.
const launchdStartGrace = 3 * time.Second

// awaitLaunchdRunning polls the job state after a kickstart until launchd
// reports it running, the grace elapses or ctx ends. It returns true only on
// a confirmed running job; an unusable answer at the end is false.
func (m *Manager) awaitLaunchdRunning(ctx context.Context) bool {
	deadline := time.Now().Add(launchdStartGrace)
	for {
		_, running, known := m.launchdState(ctx)
		if known && running {
			return true
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return false
		}
		t := time.NewTimer(100 * time.Millisecond)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return false
		}
	}
}

func (m *Manager) removeLaunchd(ctx context.Context, force bool) (Result, error) {
	var r Result
	path := filepath.Join(m.unitDir(), CollectPlist)
	if !fileExists(path) {
		r.addf("%s is not installed", CollectPlist)
		return r, nil
	}
	if !force && !hasStamp(path) {
		r.addf("refusing to remove %s: it carries no %q stamp, so aiusage did not write it; pass --force to delete it anyway",
			path, stampToken)
		r.Refused++
		return r, nil
	}
	loaded, _, known := m.launchdState(ctx)
	if !known {
		return r, fmt.Errorf("inspect %s before removal: launchd did not provide a usable answer", CollectLabel)
	}
	if loaded {
		if _, err := m.launchctl(ctx, "bootout", m.launchTarget()); err != nil {
			return r, fmt.Errorf("boot out %s: %w", CollectLabel, err)
		}
		r.change("stopped and unloaded %s", CollectLabel)
	}
	if err := os.Remove(path); err != nil {
		return r, fmt.Errorf("remove %s: %w", path, err)
	}
	r.change("removed %s", path)
	return r, nil
}

func (m *Manager) statusLaunchd(ctx context.Context) []UnitStatus {
	st := UnitStatus{
		Name:        CollectLabel,
		Installed:   fileExists(filepath.Join(m.unitDir(), CollectPlist)),
		Persistence: PersistenceGUILogin,
	}
	if !st.Installed || ctx.Err() != nil {
		return []UnitStatus{st}
	}
	st.Loaded, st.Active, st.StateKnown = m.launchdState(ctx)
	disabled, known := m.launchdDisabled(ctx)
	if !known {
		st.StateKnown = false
	} else {
		st.Enabled = !disabled
	}
	return []UnitStatus{st}
}

func (m *Manager) launchdState(ctx context.Context) (loaded, running, known bool) {
	out, err := m.launchctl(ctx, "print", m.launchTarget())
	if err == nil {
		return true, launchdRunning(out), true
	}
	if ctx.Err() != nil {
		return false, false, false
	}
	lower := strings.ToLower(out)
	for _, absent := range []string{"could not find service", "service could not be found", "not found"} {
		if strings.Contains(lower, absent) {
			return false, false, true
		}
	}
	return false, false, false
}

func launchdRunning(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "state = running" {
			return true
		}
	}
	return false
}

func (m *Manager) launchdDisabled(ctx context.Context) (disabled, known bool) {
	out, err := m.launchctl(ctx, "print-disabled", m.launchDomain())
	if err != nil {
		return false, false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, `"`+CollectLabel+`"`) {
			return strings.Contains(line, "=> true"), true
		}
	}
	return false, true
}

func (m *Manager) launchctl(ctx context.Context, args ...string) (string, error) {
	return m.command(ctx, "launchctl", args...)
}

func (m *Manager) launchDomain() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

func (m *Manager) launchTarget() string {
	return m.launchDomain() + "/" + CollectLabel
}

func writeAtomicFile(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".aiusage-restore-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
