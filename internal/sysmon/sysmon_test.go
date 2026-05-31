package sysmon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFile writes content into a synthetic cgroup tree rooted at dir.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// monAt builds a Monitor pointed at a synthetic cgroup root (no disk).
func monAt(root string) *Monitor {
	return &Monitor{cgroupRoot: root, numCPU: 8}
}

func TestMemoryV2WorkingSet(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "cgroup.controllers", "cpu memory") // marks v2
	writeFile(t, root, "memory.current", "2147483648\n")   // 2 GiB
	writeFile(t, root, "memory.stat", "anon 1073741824\ninactive_file 1073741824\n")
	writeFile(t, root, "memory.max", "4294967296\n") // 4 GiB

	g := monAt(root).memory()
	if !g.Known {
		t.Fatal("memory gauge should be known")
	}
	// working set = current(2G) - inactive_file(1G) = 1G; / 4G = 0.25.
	if g.Frac < 0.24 || g.Frac > 0.26 {
		t.Errorf("frac = %.3f, want ~0.25 (text %q)", g.Frac, g.Text)
	}
}

func TestMemoryV2UnlimitedFallsBackToHost(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "cgroup.controllers", "memory")
	writeFile(t, root, "memory.current", "1048576\n")
	writeFile(t, root, "memory.max", "max\n") // no limit

	// With "max", memoryV2 falls back to host /proc/meminfo. On the linux test
	// host that yields a known gauge; we only assert it does not crash and that
	// the limit is NOT the bogus tiny current value.
	g := monAt(root).memory()
	if g.Known && g.Frac == 1.0 {
		t.Errorf("unlimited memory should not report 100%% from current alone: %q", g.Text)
	}
}

func TestMemoryV1(t *testing.T) {
	root := t.TempDir()
	// no cgroup.controllers → v1 path
	writeFile(t, root, "memory/memory.usage_in_bytes", "536870912\n") // 512 MiB
	writeFile(t, root, "memory/memory.stat", "total_inactive_file 0\n")
	writeFile(t, root, "memory/memory.limit_in_bytes", "1073741824\n") // 1 GiB

	g := monAt(root).memory()
	if !g.Known {
		t.Fatal("v1 memory gauge should be known")
	}
	if g.Frac < 0.49 || g.Frac > 0.51 {
		t.Errorf("frac = %.3f, want ~0.5 (%q)", g.Frac, g.Text)
	}
}

func TestMemoryV1UnlimitedSentinel(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "memory/memory.usage_in_bytes", "1048576\n")
	writeFile(t, root, "memory/memory.limit_in_bytes", "9223372036854771712\n") // sentinel

	g := monAt(root).memory()
	// Falls back to host; must not report the tiny current against the sentinel.
	if g.Known && g.Frac < 0.0001 {
		t.Errorf("sentinel limit should trigger host fallback, got near-zero frac %q", g.Text)
	}
}

func TestCPUV2NeedsTwoSamples(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "cgroup.controllers", "cpu")
	writeFile(t, root, "cpu.max", "200000 100000\n") // 2 cores
	writeFile(t, root, "cpu.stat", "usage_usec 1000000\n")

	m := monAt(root)
	t0 := time.Unix(1000, 0)
	g1 := m.cpu(t0)
	if g1.Known {
		t.Fatal("first CPU sample must be Known=false (no baseline)")
	}

	// 1s later, 1s of CPU consumed on a 2-core quota → 50%.
	writeFile(t, root, "cpu.stat", "usage_usec 2000000\n")
	g2 := m.cpu(t0.Add(time.Second))
	if !g2.Known {
		t.Fatal("second CPU sample should be known")
	}
	if g2.Frac < 0.49 || g2.Frac > 0.51 {
		t.Errorf("cpu frac = %.3f, want ~0.5 (%q)", g2.Frac, g2.Text)
	}
}

func TestCPUV2CapsAt100(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "cgroup.controllers", "cpu")
	writeFile(t, root, "cpu.max", "100000 100000\n") // 1 core
	writeFile(t, root, "cpu.stat", "usage_usec 0\n")

	m := monAt(root)
	t0 := time.Unix(1000, 0)
	m.cpu(t0)
	// 5s of CPU in 1s of wall on a 1-core quota → 500% raw, clamped to 100%.
	writeFile(t, root, "cpu.stat", "usage_usec 5000000\n")
	g := m.cpu(t0.Add(time.Second))
	if g.Frac != 1.0 {
		t.Errorf("cpu frac = %.3f, want clamped 1.0", g.Frac)
	}
}

func TestDiskKnownForTempDir(t *testing.T) {
	m := New(t.TempDir())
	g := m.disk()
	if !g.Known {
		t.Fatal("disk gauge should be known for a real temp dir")
	}
	if g.Frac < 0 || g.Frac > 1 {
		t.Errorf("disk frac out of range: %.3f", g.Frac)
	}
}

func TestDiskDisabledWhenNoPath(t *testing.T) {
	if g := New("").disk(); g.Known {
		t.Error("disk gauge should be unknown when no path is set")
	}
}

func TestSampleNeverPanics(t *testing.T) {
	// A Monitor pointed at a nonexistent root must degrade to unknown gauges, not
	// crash — the TUI calls this on every tick regardless of environment.
	m := &Monitor{cgroupRoot: filepath.Join(t.TempDir(), "absent"), diskPath: "", numCPU: 4}
	_ = m.Sample()
	_ = m.Sample()
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		512:             "512B",
		1024:            "1K",
		1536:            "1.5K",
		1073741824:      "1G",
		2147483648:      "2G",
		16 * 1073741824: "16G",
	}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
