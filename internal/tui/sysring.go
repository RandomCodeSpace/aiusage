package tui

import "github.com/RandomCodeSpace/aiusage/internal/sysmon"

// sysring.go keeps the rolling resource history the Overview's sys heat strips
// draw. internal/sysmon is strictly observational and answers one INSTANTANEOUS
// snapshot per Sample() — it holds no series and is not going to grow one — so
// the history lives here, in the layer that already owns the ticker producing
// the samples. Nothing about it reaches the database: this is live machine
// state, exactly as the snapshot beside it is.

// sysRingCap is how many samples one resource keeps. At sysInterval (2s) that is
// eight minutes of history, and it is more than the widest terminal can show —
// the renderer takes the newest cells it has room for and drops the rest, so the
// capacity only has to be generous, never exact.
const sysRingCap = 240

// sysRing is a fixed-capacity ring of utilisation fractions, oldest to newest.
//
// It is a VALUE holding an array rather than a slice: Bubble Tea copies the
// Model on every message, and a slice would leave two copies appending into one
// backing array. The render memo solves that by living behind a pointer; a ring
// this small solves it by having no shared state at all.
type sysRing struct {
	buf [sysRingCap]float64
	n   int // samples ever pushed; the ring holds the last min(n, cap) of them
}

// push records one sample, overwriting the oldest once the ring is full.
func (r *sysRing) push(v float64) {
	r.buf[r.n%len(r.buf)] = v
	r.n++
}

// values returns the retained samples, oldest first. A ring that has not filled
// yet returns SHORT rather than zero-padded: a sample that was never taken is a
// hole in the strip, and a hole and an idle machine are different facts.
func (r sysRing) values() []float64 {
	n := r.n
	if n > len(r.buf) {
		n = len(r.buf)
	}
	out := make([]float64, 0, n)
	for i := r.n - n; i < r.n; i++ {
		out = append(out, r.buf[i%len(r.buf)])
	}
	return out
}

// sysHistory is the per-resource history behind the three gauges, in the same
// fixed CPU/mem/disk order the strip renders.
type sysHistory struct {
	cpu, mem, disk sysRing
}

// push records one snapshot. A gauge the monitor could not read is NOT pushed:
// an unreadable sample is not a zero one, and while it holds the gauge renders
// its unknown placeholder rather than a strip. Only CPU's first tick is normally
// affected, so the resulting cell offset between resources is a tick wide.
func (h *sysHistory) push(s sysmon.Snapshot) {
	for _, p := range []struct {
		ring  *sysRing
		gauge sysmon.Gauge
	}{{&h.cpu, s.CPU}, {&h.mem, s.Mem}, {&h.disk, s.Disk}} {
		if p.gauge.Known {
			p.ring.push(p.gauge.Frac)
		}
	}
}
