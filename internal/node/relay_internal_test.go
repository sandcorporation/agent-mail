package node

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/term"
	"github.com/sandcorporation/switchboard/internal/term/termtest"
	"github.com/sandcorporation/switchboard/internal/wire"
)

type sent struct {
	kind byte
	id   string
	b    string
}

type sink struct {
	mu  sync.Mutex
	got []sent
}

func (s *sink) send(kind byte, id string, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, sent{kind, id, string(b)})
	return nil
}

func (s *sink) frames() []sent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sent(nil), s.got...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRelaySnapshotThenStreamOnlyWhileAttached(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := termtest.New()
	name := TerminalName("hs_a")
	if err := f.Launch(ctx, term.Spec{Name: name, Argv: []string{"x"}, Cols: 20, Rows: 4}); err != nil {
		t.Fatal(err)
	}
	s := &sink{}
	r := newRelay(f, func(id string) bool { return id == "hs_a" }, s.send, t.Logf)

	// Nobody attached: output is not read.
	if err := f.Output(name, []byte("early\r\n")); err != nil {
		t.Fatal(err)
	}
	r.attach(ctx, "hs_other") // not this node's
	time.Sleep(20 * time.Millisecond)
	if f.Streams(name) != 0 || len(s.frames()) != 0 {
		t.Fatalf("streams %d, frames %v before attach", f.Streams(name), s.frames())
	}

	r.attach(ctx, "hs_a")
	waitFor(t, "snapshot", func() bool { return len(s.frames()) == 1 })
	if fr := s.frames()[0]; fr.kind != wire.TermSnapshot || fr.id != "hs_a" || !strings.Contains(fr.b, "early") {
		t.Fatalf("first frame %+v", fr)
	}
	if err := f.Output(name, []byte("late")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "output", func() bool { return len(s.frames()) == 2 })
	if fr := s.frames()[1]; fr.kind != wire.TermOutput || fr.b != "late" {
		t.Fatalf("second frame %+v", fr)
	}

	// Attaching again (another viewer) sends a fresh snapshot on the same
	// stream.
	r.attach(ctx, "hs_a")
	waitFor(t, "second snapshot", func() bool { return len(s.frames()) == 3 })
	if fr := s.frames()[2]; fr.kind != wire.TermSnapshot || !strings.Contains(fr.b, "late") || f.Streams(name) != 1 {
		t.Fatalf("third frame %+v, streams %d", fr, f.Streams(name))
	}

	// Detached: the stream closes and output is not read.
	r.detach("hs_a")
	waitFor(t, "stream closed", func() bool { return f.Streams(name) == 0 })
	n := len(s.frames())
	_ = f.Output(name, []byte("unseen"))
	time.Sleep(20 * time.Millisecond)
	if len(s.frames()) != n {
		t.Fatalf("frames after detach: %v", s.frames()[n:])
	}
}

func TestRelayExitSendsTheLastScreenAndFollowsARelaunch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := termtest.New()
	name := TerminalName("hs_a")
	if err := f.Launch(ctx, term.Spec{Name: name, Argv: []string{"x"}, Cols: 20, Rows: 4}); err != nil {
		t.Fatal(err)
	}
	s := &sink{}
	r := newRelay(f, func(string) bool { return true }, s.send, t.Logf)
	defer r.stop()
	r.attach(ctx, "hs_a")
	waitFor(t, "streaming", func() bool { return f.Streams(name) == 1 })
	_ = f.Output(name, []byte("goodbye"))
	_ = f.Exit(name, 1)
	waitFor(t, "last snapshot", func() bool {
		fr := s.frames()
		return len(fr) > 0 && fr[len(fr)-1].kind == wire.TermSnapshot && strings.Contains(fr[len(fr)-1].b, "goodbye")
	})
	if err := f.Forget(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := f.Launch(ctx, term.Spec{Name: name, Argv: []string{"x"}, Cols: 20, Rows: 4}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stream reopened", func() bool { return f.Streams(name) == 1 })
	_ = f.Output(name, []byte("again"))
	waitFor(t, "output after relaunch", func() bool {
		fr := s.frames()
		return fr[len(fr)-1].kind == wire.TermOutput && fr[len(fr)-1].b == "again"
	})
}
