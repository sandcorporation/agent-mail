package live

import (
	"slices"
	"testing"
	"time"
)

func TestResumeFromCursor(t *testing.T) {
	var b Bus
	_, r, cancel, _ := b.Subscribe("", 0)
	cancel()
	if r.Reset || r.Seq != 0 {
		t.Fatalf("first connect: %+v", r)
	}
	b.Publish("mail", "progress", "mail")
	b.Publish("questions")
	b.Publish("sessions")
	_, r, cancel, _ = b.Subscribe(b.Boot(), 1)
	cancel()
	if r.Reset || r.Seq != 3 || !slices.Equal(r.Kinds, []string{"questions", "sessions"}) {
		t.Fatalf("resume after 1: %+v", r)
	}
	if _, r, cancel, _ = b.Subscribe(b.Boot(), 3); r.Reset || len(r.Kinds) != 0 {
		t.Fatalf("up to date: %+v", r)
	}
	cancel()
	for _, c := range []struct {
		boot  string
		after uint64
	}{{"other", 1}, {b.Boot(), 9}} {
		if _, r, cancel, _ = b.Subscribe(c.boot, c.after); !r.Reset {
			t.Fatalf("%+v: want reset, got %+v", c, r)
		}
		cancel()
	}
	for range ringSize + 1 {
		b.Publish("mail")
	}
	if _, r, cancel, _ = b.Subscribe(b.Boot(), 1); !r.Reset {
		t.Fatalf("cursor older than the ring: %+v", r)
	}
	cancel()
}

// A subscriber that never reads is dropped; publishers never block.
func TestSlowSubscriberIsDropped(t *testing.T) {
	var b Bus
	slow, _, cancelSlow, _ := b.Subscribe("", 0)
	defer cancelSlow()
	fast, _, cancelFast, _ := b.Subscribe("", 0)
	defer cancelFast()
	for i := range queueSize * 3 {
		b.Publish("mail")
		select {
		case ev := <-fast.C:
			if ev.Seq != uint64(i+1) {
				t.Fatalf("fast got seq %d, want %d", ev.Seq, i+1)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the reading subscriber missed an event")
		}
	}
	n := 0
	for range slow.C {
		n++
	}
	if n != queueSize {
		t.Fatalf("slow subscriber got %d before being dropped", n)
	}
	if b.Clients() != 1 {
		t.Fatalf("clients = %d", b.Clients())
	}
}
