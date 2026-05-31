// Package sysmon reads the resource usage of the CURRENT container/pod — not the
// whole host — so the TUI's CPU/memory/disk gauges reflect the devpod the user
// actually runs in. It prefers cgroup v2, falls back to cgroup v1, and finally
// to host-wide /proc when no cgroup limit is set.
//
// Everything here is read-only and pure-Go (CGO_ENABLED=0 safe): plain file
// reads under the cgroup mount plus a stdlib syscall.Statfs for disk. No new
// dependency, no writes, and it never touches the aiusage database.
package sysmon

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Gauge is one resource reading for a usage bar. Frac is the 0..1 fill; Text is
// a short human readout (e.g. "1.2G/4.0G" or "0.4/2 cpu"). Known is false when
// the value could not be determined (e.g. CPU before the second sample), so the
// caller renders a placeholder instead of a misleading 0%.
type Gauge struct {
	Frac  float64
	Text  string
	Known bool
}

// Snapshot bundles the three gauges produced by one Monitor.Sample call.
type Snapshot struct {
	CPU  Gauge
	Mem  Gauge
	Disk Gauge
}

// Monitor samples container resource usage over time. CPU is a rate, so it needs
// two samples to produce a value; the Monitor holds the previous CPU reading
// between calls. Construct with New and call Sample on each tick.
type Monitor struct {
	cgroupRoot string // override point for tests; defaults to /sys/fs/cgroup
	diskPath   string // filesystem to statfs for the disk gauge
	numCPU     int    // host CPU count, used when no CPU quota is set

	// mu guards the CPU rate state below. The TUI shares a single *Monitor across
	// bubbletea's value-copied Model, and Sample() runs from tick-driven Update
	// handling, so the prev* fields are read/written from more than one goroutine
	// over the program's life — the lock makes that access data-race-free.
	mu          sync.Mutex
	prevCPUUsec int64     // cgroup cpu usage (microseconds) at last sample
	prevAt      time.Time // wall clock at last sample
	havePrev    bool
}

// New returns a Monitor whose disk gauge measures the filesystem containing
// diskPath (typically the workspace dir). An empty diskPath disables the disk
// gauge (Known=false).
func New(diskPath string) *Monitor {
	return &Monitor{
		cgroupRoot: "/sys/fs/cgroup",
		diskPath:   diskPath,
		numCPU:     runtime.NumCPU(),
	}
}

// Sample reads the current CPU/memory/disk usage. CPU is the average over the
// interval since the previous Sample; the first call returns CPU.Known=false.
func (m *Monitor) Sample() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	var s Snapshot
	s.Mem = m.memory()
	s.Disk = m.disk()
	s.CPU = m.cpu(now)
	return s
}

// v2 reports whether the cgroup mount is the unified (v2) hierarchy.
func (m *Monitor) v2() bool {
	_, err := os.Stat(m.cgroupRoot + "/cgroup.controllers")
	return err == nil
}

// --- memory ---

func (m *Monitor) memory() Gauge {
	if m.v2() {
		return m.memoryV2()
	}
	return m.memoryV1()
}

// memoryV2 uses the working set (memory.current - inactive_file), the same basis
// cAdvisor/kubectl-top report, against memory.max. When memory.max is "max" (no
// limit) it falls back to host /proc/meminfo so the gauge still shows something.
func (m *Monitor) memoryV2() Gauge {
	cur, ok := readInt64(m.cgroupRoot + "/memory.current")
	if !ok {
		return m.memoryProc()
	}
	used := cur
	if inactive, ok := statField(m.cgroupRoot+"/memory.stat", "inactive_file"); ok && inactive <= cur {
		used = cur - inactive
	}
	limit, ok := readCgroupMax(m.cgroupRoot + "/memory.max")
	if !ok || limit <= 0 {
		return m.memoryProc() // unlimited container → show host memory
	}
	return memGauge(used, limit)
}

// memoryV1 reads the legacy memory controller. limit_in_bytes uses a huge
// sentinel (~ PAGE_COUNTER_MAX*page) when unlimited, which we treat as no limit.
func (m *Monitor) memoryV1() Gauge {
	base := m.cgroupRoot + "/memory"
	cur, ok := readInt64(base + "/memory.usage_in_bytes")
	if !ok {
		return m.memoryProc()
	}
	used := cur
	if inactive, ok := statField(base+"/memory.stat", "total_inactive_file"); ok && inactive <= cur {
		used = cur - inactive
	}
	limit, ok := readInt64(base + "/memory.limit_in_bytes")
	if !ok || limit <= 0 || limit >= unlimitedV1 {
		return m.memoryProc()
	}
	return memGauge(used, limit)
}

// memoryProc derives a gauge from host /proc/meminfo (used = MemTotal -
// MemAvailable). Used when no cgroup memory limit applies.
func (m *Monitor) memoryProc() Gauge {
	total, okT := meminfoField("MemTotal")
	avail, okA := meminfoField("MemAvailable")
	if !okT || !okA || total <= 0 {
		return Gauge{}
	}
	used := total - avail
	if used < 0 {
		used = 0
	}
	return memGauge(used, total)
}

func memGauge(used, limit int64) Gauge {
	if limit <= 0 {
		return Gauge{}
	}
	return Gauge{
		Frac:  clamp01(float64(used) / float64(limit)),
		Text:  humanBytes(used) + "/" + humanBytes(limit),
		Known: true,
	}
}

// --- cpu ---

// cpu computes the CPU usage fraction over the interval since the previous
// sample, normalised by the container's CPU quota (cores). The first call only
// records the baseline and returns Known=false.
func (m *Monitor) cpu(now time.Time) Gauge {
	usec, cores, ok := m.cpuUsageAndCores()
	if !ok {
		return Gauge{}
	}
	prevUsec, prevAt, had := m.prevCPUUsec, m.prevAt, m.havePrev
	m.prevCPUUsec, m.prevAt, m.havePrev = usec, now, true
	if !had {
		return Gauge{Text: cpuText(0, cores) + " …", Known: false}
	}
	wall := now.Sub(prevAt).Microseconds()
	if wall <= 0 || cores <= 0 {
		return Gauge{Known: false}
	}
	frac := float64(usec-prevUsec) / (float64(wall) * cores)
	frac = clamp01(frac)
	return Gauge{Frac: frac, Text: cpuText(frac*cores, cores), Known: true}
}

// cpuUsageAndCores returns cumulative CPU time (microseconds) and the effective
// core count (quota/period, or host CPUs when unlimited), preferring cgroup v2.
func (m *Monitor) cpuUsageAndCores() (usec int64, cores float64, ok bool) {
	if m.v2() {
		u, ok := statField(m.cgroupRoot+"/cpu.stat", "usage_usec")
		if !ok {
			return 0, 0, false
		}
		return u, m.coresV2(), true
	}
	// v1: cpuacct.usage is nanoseconds.
	ns, ok := readInt64(m.cgroupRoot + "/cpuacct/cpuacct.usage")
	if !ok {
		return 0, 0, false
	}
	return ns / 1000, m.coresV1(), true
}

// coresV2 parses "cpu.max" = "<quota> <period>"; "max" period means no quota, so
// the container can use all host CPUs.
func (m *Monitor) coresV2() float64 {
	data, err := os.ReadFile(m.cgroupRoot + "/cpu.max")
	if err != nil {
		return float64(m.numCPU)
	}
	f := strings.Fields(strings.TrimSpace(string(data)))
	if len(f) != 2 || f[0] == "max" {
		return float64(m.numCPU)
	}
	quota, e1 := strconv.ParseInt(f[0], 10, 64)
	period, e2 := strconv.ParseInt(f[1], 10, 64)
	if e1 != nil || e2 != nil || quota <= 0 || period <= 0 {
		return float64(m.numCPU)
	}
	return float64(quota) / float64(period)
}

// coresV1 derives cores from cfs_quota_us / cfs_period_us; a quota of -1 means
// unlimited.
func (m *Monitor) coresV1() float64 {
	quota, ok := readInt64(m.cgroupRoot + "/cpu/cpu.cfs_quota_us")
	if !ok || quota <= 0 {
		return float64(m.numCPU)
	}
	period, ok := readInt64(m.cgroupRoot + "/cpu/cpu.cfs_period_us")
	if !ok || period <= 0 {
		return float64(m.numCPU)
	}
	return float64(quota) / float64(period)
}

func cpuText(usedCores, cores float64) string {
	return trim1(usedCores) + "/" + trim1(cores) + " cpu"
}

// --- disk ---

// disk reports usage of the filesystem holding m.diskPath via statfs. This is a
// filesystem-level number (cgroups do not cap disk bytes), but scoping it to the
// workspace path keeps it relevant to the devpod rather than a host overlay.
func (m *Monitor) disk() Gauge {
	if m.diskPath == "" {
		return Gauge{}
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(m.diskPath, &st); err != nil {
		return Gauge{}
	}
	bs := int64(st.Bsize)
	total := int64(st.Blocks) * bs
	avail := int64(st.Bavail) * bs
	if total <= 0 {
		return Gauge{}
	}
	used := total - avail // space usable space-wise from this path's view
	if used < 0 {
		used = 0
	}
	return Gauge{
		Frac:  clamp01(float64(used) / float64(total)),
		Text:  humanBytes(used) + "/" + humanBytes(total),
		Known: true,
	}
}

// --- file helpers ---

const unlimitedV1 = int64(0x7FFFFFFFFFFFF000) // cgroup v1 "no limit" sentinel

// readInt64 reads a file whose entire contents are a single integer.
func readInt64(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// readCgroupMax reads a cgroup v2 limit file that is either an integer or the
// literal "max" (meaning no limit → ok=false).
func readCgroupMax(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(data))
	if s == "max" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// statField scans a "key value" file (cpu.stat, memory.stat) for key and returns
// its integer value.
func statField(path, key string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == key {
			if n, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// meminfoField reads a "/proc/meminfo" field (value is in kB) and returns bytes.
func meminfoField(key string) (int64, bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, key+":") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				if n, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					return n * 1024, true
				}
			}
		}
	}
	return 0, false
}

// --- formatting ---

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// humanBytes formats a byte count with a binary-ish suffix, one decimal for
// sub-10 magnitudes (e.g. "1.2G", "512M", "4.0G").
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + "B"
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	val := float64(n) / float64(div)
	suffix := []string{"K", "M", "G", "T", "P"}[exp]
	if val < 10 {
		return trim1(val) + suffix
	}
	return strconv.FormatInt(int64(val+0.5), 10) + suffix
}

// trim1 formats a float with at most one decimal, dropping a trailing ".0".
func trim1(f float64) string {
	s := strconv.FormatFloat(f, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}
