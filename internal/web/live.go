package web

import (
	"context"
	"sync"
	"time"
)

// broadcaster fans the ledger watermark out to every open event stream. The
// watermark is the observed time of the newest usage row, which the collector
// moves once per pass that landed anything; a pass that found nothing new
// leaves it alone and the browser hears nothing, which is the correct amount.
type broadcaster struct {
	mu   sync.Mutex
	last time.Time
	subs map[chan time.Time]struct{}
}

func newBroadcaster() *broadcaster {
	return &broadcaster{subs: make(map[chan time.Time]struct{})}
}

func (b *broadcaster) current() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last
}

func (b *broadcaster) set(wm time.Time) {
	b.mu.Lock()
	b.last = wm
	b.mu.Unlock()
}

// subscribe returns a channel that receives each new watermark and the call
// that removes it. The channel holds one value and a send never blocks: a
// subscriber that has not drained its last push already has a refetch pending,
// and that refetch reads the ledger as it stands, so a dropped intermediate
// value costs nothing.
func (b *broadcaster) subscribe() (<-chan time.Time, func()) {
	ch := make(chan time.Time, 1)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// poll re-reads the watermark every interval and publishes it when it moved.
// A read error is skipped rather than published: the next tick retries, and a
// transient failure must not look like new data.
func (b *broadcaster) poll(ctx context.Context, src Source, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		wm, err := src.IngestWatermark(ctx)
		if err != nil {
			continue
		}
		b.mu.Lock()
		if wm.Equal(b.last) {
			b.mu.Unlock()
			continue
		}
		b.last = wm
		for ch := range b.subs {
			select {
			case ch <- wm:
			default:
			}
		}
		b.mu.Unlock()
	}
}
