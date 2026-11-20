package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

type sent struct {
	lines string
	count int
}

// fakeWindows drives runBatches with a clock the test advances: each window
// waits on a channel the test closes.
type fakeWindows struct{ ticks chan chan time.Time }

func (f fakeWindows) after(time.Duration) <-chan time.Time {
	c := make(chan time.Time, 1)
	f.ticks <- c
	return c
}

func startBatches(t *testing.T) (*lineBatcher, fakeWindows, chan sent, chan struct{}) {
	t.Helper()
	b := newLineBatcher()
	f := fakeWindows{ticks: make(chan chan time.Time)}
	out := make(chan sent, 10)
	done := make(chan struct{})
	go func() {
		runBatches(b, time.Minute, f.after, func(lines string, count int) { out <- sent{lines, count} })
		close(done)
	}()
	return b, f, out, done
}

func TestLineBatcherOneSendPerWindow(t *testing.T) {
	b, f, out, done := startBatches(t)
	_, _ = b.Write([]byte("l1\nl2\nl"))
	_, _ = b.Write([]byte("3\nl4\nl5\n"))
	tick := <-f.ticks // the first line opened a window
	select {
	case s := <-out:
		t.Fatalf("sent before the window closed: %+v", s)
	default:
	}
	tick <- time.Time{}
	if s := <-out; s.count != 5 || s.lines != "l1\nl2\nl3\nl4\nl5" {
		t.Fatalf("first window: %+v", s)
	}
	_, _ = b.Write([]byte("l6\n"))
	(<-f.ticks) <- time.Time{}
	if s := <-out; s.count != 1 || s.lines != "l6" {
		t.Fatalf("second window: %+v", s)
	}
	// Close sends what is left at once, a last unterminated line included,
	// without waiting out the window.
	_, _ = b.Write([]byte("l7\nl8"))
	<-f.ticks
	b.Close()
	if s := <-out; s.count != 2 || s.lines != "l7\nl8" {
		t.Fatalf("on close: %+v", s)
	}
	<-done
}

func TestLineBatcherCapsTheLinesButCountsAll(t *testing.T) {
	b, f, out, done := startBatches(t)
	_, _ = b.Write([]byte(strings.Repeat("x\n", wire.MaxJobTailLines+7)))
	_, _ = b.Write([]byte(strings.Repeat("y", wire.MaxJobTailBytes) + "\n"))
	(<-f.ticks) <- time.Time{}
	s := <-out
	if s.count != wire.MaxJobTailLines+8 || strings.Count(s.lines, "x") != wire.MaxJobTailLines || strings.Contains(s.lines, "y") {
		t.Fatalf("capped: count %d, %d x", s.count, strings.Count(s.lines, "x"))
	}
	b.Close()
	<-done
}

func TestLineBatcherCloseWithNothingSendsNothing(t *testing.T) {
	b, _, out, done := startBatches(t)
	b.Close()
	<-done
	if len(out) != 0 {
		t.Fatalf("sent: %+v", <-out)
	}
}
