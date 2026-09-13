package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWayfinderCollectorIdentity(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{"XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "AIUSAGE_DB", "AIUSAGE_HOME"} {
		t.Setenv(name, "")
	}
	t.Setenv("HOME", home)
	base, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if err := base.ResolveCollectorPaths(); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(base.PIDPath) != "aiusage.pid" || base.LegacyPIDPath() != "" {
		t.Fatalf("default moved: %+v", base)
	}
	seen := map[string]bool{base.PIDPath: true}
	for _, database := range []string{"scratch-a.db", "scratch-b.db"} {
		c := base
		c.DBPath = filepath.Join(home, database)
		if err := c.ResolveCollectorPaths(); err != nil {
			t.Fatal(err)
		}
		if seen[c.PIDPath] || c.LegacyPIDPath() != base.PIDPath || c.LogPath != base.LogPath {
			t.Fatalf("isolation failure: %+v", c)
		}
		seen[c.PIDPath] = true
		again := c
		if err := again.ResolveCollectorPaths(); err != nil {
			t.Fatal(err)
		}
		if again.PIDPath != c.PIDPath {
			t.Fatal("identity is unstable")
		}
	}
	path := filepath.Join(home, "config.json")
	if err := os.WriteFile(path, []byte(`{"pid_path":"`+base.PIDPath+`","db_path":"scratch.db"}`), 0600); err != nil {
		t.Fatal(err)
	}
	pinned, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := pinned.ResolveCollectorPaths(); err != nil {
		t.Fatal(err)
	}
	if pinned.PIDPath != base.PIDPath || pinned.LegacyPIDPath() != "" {
		t.Fatal("explicit default-valued PID was not preserved")
	}
}

func TestWayfinderRejectEmptyPID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"pid_path":""}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "pid_path") {
		t.Fatalf("Load = %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), ".lock")); !os.IsNotExist(err) {
		t.Fatal("invalid PID created lock")
	}
}
