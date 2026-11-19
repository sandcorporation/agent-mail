package hosting

import "testing"

func TestTallySumsInputPerMinute(t *testing.T) {
	var y tally
	minute := LogWindow.Milliseconds()
	if d := y.add(1000, 5); d.bytes != 0 {
		t.Fatalf("first input flushed %+v", d)
	}
	if d := y.add(1000+minute-1, 3); d.bytes != 0 {
		t.Fatalf("input within the minute flushed %+v", d)
	}
	if d := y.due(1000+minute-1, false); d.bytes != 0 {
		t.Fatalf("due before the minute is over: %+v", d)
	}
	// The next minute's first input closes the window.
	d := y.add(1000+minute, 2)
	if d != (tally{start: 1000, bytes: 8}) {
		t.Fatalf("closed window %+v", d)
	}
	// A quiet socket's window is taken by the periodic check.
	if d := y.due(1000+2*minute, false); d != (tally{start: 1000 + minute, bytes: 2}) {
		t.Fatalf("due %+v", d)
	}
	if d := y.due(1000+3*minute, true); d.bytes != 0 {
		t.Fatalf("empty window logged %+v", d)
	}
	y.add(5, 1)
	if d := y.due(5, true); d != (tally{start: 5, bytes: 1}) {
		t.Fatalf("forced %+v", d)
	}
}
