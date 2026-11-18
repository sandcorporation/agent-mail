// Package live is the dashboard's change bus: a numbered stream of change
// kinds (dashboard tab names) that the server publishes after every write and
// that WebSocket clients follow. An event carries no data; clients re-read the
// admin API, so SQLite stays the only source of truth.
//
// Publish never blocks: each subscriber has a bounded queue, and a subscriber
// whose queue is full is dropped (its channel closed). A dropped or
// reconnecting client passes its last seq and resumes from the ring of recent
// events, or is told to reload everything when the ring no longer covers it
// or the server restarted (another boot id).
package live

import (
	"crypto/rand"
	"encoding/hex"
	"slices"
	"sync"
)

// Event is one change: Seq increases by one per Publish within a boot.
type Event struct {
	Seq   uint64   `json:"seq"`
	Kinds []string `json:"kinds"`
}

const (
	ringSize   = 512
	queueSize  = 64
	maxClients = 1000
)

// Bus is safe for concurrent use. The zero value is ready to use.
type Bus struct {
	boot string
	mu   sync.Mutex
	seq  uint64
	ring []Event // the last ringSize events, oldest first
	subs map[*Sub]struct{}
}

// Sub is one subscriber. C is closed when the subscriber is dropped (too slow)
// or cancelled.
type Sub struct {
	C    <-chan Event
	ch   chan Event
	done bool
}

func (b *Bus) initLocked() {
	if b.boot == "" {
		id := make([]byte, 8)
		_, _ = rand.Read(id)
		b.boot = hex.EncodeToString(id)
		b.subs = map[*Sub]struct{}{}
	}
}

// Boot identifies this bus (random per process); sequence numbers only
// compare within one boot.
func (b *Bus) Boot() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.initLocked()
	return b.boot
}

// Publish records one event with the given kinds (duplicates removed) and
// hands it to every subscriber without blocking.
func (b *Bus) Publish(kinds ...string) {
	if len(kinds) == 0 {
		return
	}
	k := slices.Clone(kinds)
	slices.Sort(k)
	k = slices.Compact(k)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.initLocked()
	b.seq++
	ev := Event{Seq: b.seq, Kinds: k}
	b.ring = append(b.ring, ev)
	if len(b.ring) > ringSize {
		b.ring = slices.Clone(b.ring[len(b.ring)-ringSize:])
	}
	for s := range b.subs {
		select {
		case s.ch <- ev:
		default: // too slow: drop it; it resumes from its cursor
			b.dropLocked(s)
		}
	}
}

func (b *Bus) dropLocked(s *Sub) {
	if !s.done {
		s.done = true
		close(s.ch)
		delete(b.subs, s)
	}
}

// Resume is what a subscriber missed since a cursor.
type Resume struct {
	Seq   uint64   // the bus's current seq
	Reset bool     // the cursor is from another boot or older than the ring: reload everything
	Kinds []string // kinds changed after the cursor (when not Reset)
}

// Subscribe registers a subscriber. boot and after are the client's cursor
// ("" for a first connection, which needs no resume). ok is false when the
// bus already has maxClients subscribers.
func (b *Bus) Subscribe(boot string, after uint64) (sub *Sub, r Resume, cancel func(), ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.initLocked()
	if len(b.subs) >= maxClients {
		return nil, Resume{}, nil, false
	}
	r.Seq = b.seq
	if boot != "" {
		switch {
		case boot != b.boot || after > b.seq:
			r.Reset = true
		case after < b.seq:
			if len(b.ring) == 0 || b.ring[0].Seq > after+1 {
				r.Reset = true
				break
			}
			for _, ev := range b.ring {
				if ev.Seq > after {
					r.Kinds = append(r.Kinds, ev.Kinds...)
				}
			}
			slices.Sort(r.Kinds)
			r.Kinds = slices.Compact(r.Kinds)
		}
	}
	ch := make(chan Event, queueSize)
	s := &Sub{C: ch, ch: ch}
	b.subs[s] = struct{}{}
	return s, r, func() {
		b.mu.Lock()
		b.dropLocked(s)
		b.mu.Unlock()
	}, true
}

// Clients is the number of live subscribers.
func (b *Bus) Clients() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
