package hosting

import (
	"testing"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// The host keeps the newest EventKeep events per session, ignores a repeat
// and an event of a session it does not have, and drops a session's events
// with it (#252).
func TestEventsAreBoundedDedupedAndDroppedWithTheSession(t *testing.T) {
	s := newService(t)
	h, err := s.Create(record())
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.Create(wire.HostedSessionCreate{Node: "pc", Base: "team/worker", Profile: "p", Directory: "/work/other"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range EventKeep + 5 {
		e := wire.HostedEvent{ID: h.ID, At: int64(1000 + i), Kind: wire.HostedEventRestart, Restarts: i, Actor: wire.HostedActorPolicy}
		for range 2 { // the node sends what it holds again on a new connection
			if err := s.recordEvent(e); err != nil {
				t.Fatal(err)
			}
		}
	}
	code := 3
	if err := s.recordEvent(wire.HostedEvent{ID: other.ID, At: 5, Kind: wire.HostedEventCrash, ExitCode: &code}); err != nil {
		t.Fatal(err)
	}
	if err := s.recordEvent(wire.HostedEvent{ID: "hs_" + "0123456789abcdef0123456789abcdef", At: 5, Kind: wire.HostedEventStart}); err != nil {
		t.Fatal(err)
	}
	ev, err := s.Events(h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != EventKeep || ev[0].Restarts != EventKeep+4 || ev[len(ev)-1].Restarts != 5 {
		t.Fatalf("%d events, newest %d, oldest %d", len(ev), ev[0].Restarts, ev[len(ev)-1].Restarts)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM hosted_events`).Scan(&n); err != nil || n != EventKeep+1 {
		t.Fatalf("rows %d (%v), want %d", n, err, EventKeep+1)
	}
	if ev, _ := s.Events(other.ID); len(ev) != 1 || ev[0].ExitCode == nil || *ev[0].ExitCode != 3 {
		t.Fatalf("other session %+v", ev)
	}
	if _, err := s.Delete(h.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM hosted_events WHERE session=?`, h.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows after delete %d (%v)", n, err)
	}
}
