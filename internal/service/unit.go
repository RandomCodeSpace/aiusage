package service

import (
	"io"
	"os"
	"path/filepath"
	"strings"
)

// CollectUnit supervises the collection daemon (aiusage run). The name is fixed
// rather than derived: a machine that was set up by hand already carries this
// file, and a generated install has to land on it instead of creating a second
// unit under a different name that would fight the first for the collection
// lock.
const CollectUnit = "aiusage-collect.service"

// docURL is the Documentation= line the unit carries.
const docURL = "https://github.com/RandomCodeSpace/aiusage"

// unitStamp is the comment line every unit this package renders begins with,
// and the only evidence that aiusage wrote a given file.
//
// It exists for removal. Install is create-if-missing precisely because a unit
// file may be the user's own - hand-written before this feature existed, or
// edited since - and a --remove that deleted those would be taking back
// something it never gave. The stamp is what tells the two apart; systemd
// ignores comment lines, so carrying it costs nothing.
const unitStamp = "# aiusage-generated-unit"

// generatedNote heads every rendered unit. Install is create-if-missing, so
// once a file exists it belongs to whoever edits it next; the note says where
// it came from and what will not silently overwrite it.
const generatedNote = unitStamp + "\n" +
	"# Written by aiusage setup.\n" +
	"# An existing unit is never rewritten; use `aiusage setup --force` to replace it.\n" +
	"# Deleting the line above makes `aiusage setup --remove` treat this file as yours.\n"

// stampScan bounds how much of a unit file hasStamp reads. The stamp is on the
// first line of anything aiusage wrote, unit files are small, and a path in the
// unit directory can nonetheless be anything at all.
const stampScan = 4 << 10

// hasStamp reports whether the file at path carries the generated-unit stamp.
// An unreadable file is not stamped: removal must refuse what it cannot vouch
// for.
func hasStamp(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	head, err := io.ReadAll(io.LimitReader(f, stampScan))
	if err != nil {
		return false
	}
	return strings.Contains(string(head), unitStamp)
}

// hardening is the unit's sandbox. It is copied from the hand-written unit this
// feature replaces: the collector only ever reads the agent CLIs' own files and
// writes inside its own data and state directories, so everything else on the
// filesystem can stay read-only to it.
const hardening = "NoNewPrivileges=yes\n" +
	"PrivateTmp=yes\n" +
	"ProtectSystem=strict\n" +
	"ProtectKernelTunables=yes\n" +
	"ProtectControlGroups=yes\n" +
	"RestrictSUIDSGID=yes\n"

// Options describes the supervision to install on this machine.
type Options struct {
	// Exec is the absolute path of the aiusage binary the units run. Resolve it
	// with SelfExec: a unit pointing at a symlink or a relative path outlives
	// the shell that had the context to interpret it.
	Exec string

	// Args are the global flags forwarded into the unit (--db, --config,
	// --home, --interval), already in flag/value order. The automatic install
	// passes none: see the override rule in internal/cmd.
	Args []string

	// DataDir holds the database; StateDir holds the pidfile, lock and log.
	// Both land in ReadWritePaths, which is what makes ProtectSystem=strict
	// survivable - SQLite writes the WAL and shm sidecars beside the database,
	// so the directories, not the files, have to be writable.
	DataDir  string
	StateDir string

	// Force rewrites a unit file that already exists. Left off, install is
	// create-if-missing and a unit the user has edited stays theirs.
	Force bool
}

// DefaultUnitDir returns the systemd user unit directory:
// $XDG_CONFIG_HOME/systemd/user, or ~/.config/systemd/user when that variable
// is unset or (per the XDG spec) relative.
func DefaultUnitDir() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(dir) {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "systemd", "user")
}

// SelfExec returns the absolute, symlink-resolved path of the running binary.
// The symlink step matters for the usual install shapes: ~/.local/bin/aiusage
// pointing into a versioned directory, or a package manager's shim. A unit that
// baked the link would keep working, but it would also keep working after the
// link was repointed at something else, which is not what the user who ran the
// install meant.
func SelfExec() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	// A path that cannot be resolved (a deleted or unreadable target) is still
	// the best name we have for this binary, and refusing to install over it
	// would be worse than installing the unresolved spelling.
	return exe, nil
}

// renderCollect renders the collection unit.
func renderCollect(o Options) string {
	var b strings.Builder
	b.WriteString(generatedNote)
	b.WriteString("\n[Unit]\n")
	b.WriteString("Description=aiusage collection daemon\n")
	b.WriteString("Documentation=" + docURL + "\n")
	b.WriteString("After=network.target\n")
	b.WriteString("StartLimitIntervalSec=300\n")
	b.WriteString("StartLimitBurst=5\n")
	b.WriteString("\n[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("WorkingDirectory=%h\n")
	// The daemon holds the collection lock, so exactly one instance may run. It
	// re-execs itself in place when the binary is replaced, keeping the same
	// pid, which systemd sees as the same process rather than a restart.
	b.WriteString("ExecStart=" + execStart(o, "run") + "\n")
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=10\n\n")
	b.WriteString(hardening)
	b.WriteString("ReadWritePaths=" + readWritePaths(o.DataDir, o.StateDir) + "\n")
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// execStart renders one ExecStart line: the absolute binary, its subcommand,
// then the global flags the installing invocation forwarded.
func execStart(o Options, sub string) string {
	parts := make([]string, 0, len(o.Args)+2)
	parts = append(parts, quote(o.Exec), sub)
	for _, a := range o.Args {
		parts = append(parts, quote(a))
	}
	return strings.Join(parts, " ")
}

// readWritePaths renders a ReadWritePaths value, dropping empties and repeats.
// The two directories coincide on an install that keeps state beside data, and
// listing one path twice is noise in a file people read.
func readWritePaths(paths ...string) string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		if p == "" || p == "." || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, quote(p))
	}
	return strings.Join(out, " ")
}

// quote renders one word of a unit file. systemd splits values on whitespace
// and expands % specifiers, so a home directory with a space in it or a
// database path holding a percent sign would otherwise mean something different
// to the service manager than it did to the user who typed it.
func quote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
