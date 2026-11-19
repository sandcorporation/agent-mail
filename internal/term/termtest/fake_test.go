package termtest_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/term"
	"github.com/sandcorporation/switchboard/internal/term/termtest"
)

// node stands in for a node process: it only knows the Backend interface.
type node struct{ b term.Backend }

func (n node) alive(t *testing.T, name string) term.Info {
	t.Helper()
	infos, err := n.b.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range infos {
		if in.Name == name {
			return in
		}
	}
	t.Fatalf("%s not listed in %+v", name, infos)
	return term.Info{}
}

func launch(t *testing.T, b term.Backend, name string) {
	t.Helper()
	err := b.Launch(context.Background(), term.Spec{Name: name, Argv: []string{"agent", "--session-id", "x"}, Cols: 20, Rows: 4})
	if err != nil {
		t.Fatal(err)
	}
}

func screenLine(t *testing.T, b term.Backend, name string, y int) string {
	t.Helper()
	g, err := b.Screen(context.Background(), name, 0)
	if err != nil {
		t.Fatal(err)
	}
	var s strings.Builder
	for _, c := range g.Screen[y] {
		s.WriteString(c.Text)
	}
	return strings.TrimRight(s.String(), " ")
}

func TestScriptedOutputReachesScreenAndStream(t *testing.T) {
	ctx := context.Background()
	f := termtest.New()
	launch(t, f, "sb-1")
	if err := f.Output("sb-1", []byte("before")); err != nil {
		t.Fatal(err)
	}
	r, err := f.Stream(ctx, "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Output("sb-1", []byte("\r\n\x1b[1mafter")); err != nil {
		t.Fatal(err)
	}
	if got := screenLine(t, f, "sb-1", 1); got != "after" {
		t.Errorf("screen line 1 = %q", got)
	}
	if err := f.Exit("sb-1", 0); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "\r\n\x1b[1mafter" {
		t.Errorf("stream = %q, want only output after Stream", got)
	}
}

func TestInputIsCapturedAndAnswered(t *testing.T) {
	ctx := context.Background()
	f := termtest.New()
	f.SetResponder(func(name string, in []byte) []byte { return bytes.ToUpper(in) })
	launch(t, f, "sb-1")
	for _, s := range []string{"hel", "lo\r"} {
		if err := f.Input(ctx, "sb-1", []byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if got := string(f.Typed("sb-1")); got != "hello\r" {
		t.Errorf("typed = %q", got)
	}
	if got := screenLine(t, f, "sb-1", 0); got != "HELLO" {
		t.Errorf("screen = %q", got)
	}
}

func TestExitCodesAndStop(t *testing.T) {
	ctx := context.Background()
	f := termtest.New()
	launch(t, f, "sb-1")
	launch(t, f, "sb-2")
	if err := f.Exit("sb-1", 3); err != nil {
		t.Fatal(err)
	}
	if err := f.Stop(ctx, "sb-2", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	n := node{f}
	if in := n.alive(t, "sb-1"); in.Alive || in.ExitCode != 3 {
		t.Errorf("sb-1 = %+v, want dead with 3", in)
	}
	if in := n.alive(t, "sb-2"); in.Alive || in.ExitCode != termtest.StopExitCode {
		t.Errorf("sb-2 = %+v, want stopped", in)
	}
	if got := f.Stops(); len(got) != 1 || got[0] != (termtest.Stop{Name: "sb-2", Grace: 10 * time.Second}) {
		t.Errorf("stops = %+v", got)
	}
	if err := f.Input(ctx, "sb-1", []byte("x")); !errors.Is(err, term.ErrNotRunning) {
		t.Errorf("input to dead session: %v", err)
	}
	// The name stays taken until Forget.
	if err := f.Launch(ctx, term.Spec{Name: "sb-1"}); !errors.Is(err, term.ErrSessionExists) {
		t.Errorf("relaunch before forget: %v", err)
	}
	if err := f.Forget(ctx, "sb-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Screen(ctx, "sb-1", 0); !errors.Is(err, term.ErrNoSession) {
		t.Errorf("screen after forget: %v", err)
	}
	launch(t, f, "sb-1")
	if len(f.Launches()) != 3 {
		t.Errorf("launches = %d, want 3", len(f.Launches()))
	}
}

func TestForgetRefusesLiveSession(t *testing.T) {
	f := termtest.New()
	launch(t, f, "sb-1")
	if err := f.Forget(context.Background(), "sb-1"); err == nil {
		t.Fatal("forgot a live session")
	}
}

func TestResizesAreRecorded(t *testing.T) {
	ctx := context.Background()
	f := termtest.New()
	launch(t, f, "sb-1")
	for _, s := range []termtest.Size{{30, 10}, {12, 5}} {
		if err := f.Resize(ctx, "sb-1", s.Cols, s.Rows); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.Resizes("sb-1"); len(got) != 2 || got[0] != (termtest.Size{30, 10}) || got[1] != (termtest.Size{12, 5}) {
		t.Errorf("resizes = %+v", got)
	}
	g, err := f.Screen(ctx, "sb-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if g.Cols != 12 || g.Rows != 5 {
		t.Errorf("screen size %dx%d, want 12x5", g.Cols, g.Rows)
	}
}

func TestSessionsSurviveNodeRestart(t *testing.T) {
	f := termtest.New()
	start := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	f.SetClock(func() time.Time { return start })

	// The first node launches a session, watches it, then goes away.
	ctx1, stop1 := context.WithCancel(context.Background())
	first := node{f}
	launch(t, first.b, "sb-7")
	r, err := first.b.Stream(ctx1, "sb-7")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Output("sb-7", []byte("working")); err != nil {
		t.Fatal(err)
	}
	before := first.alive(t, "sb-7")
	stop1()
	if _, err := io.ReadAll(r); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("stream after node exit: %v", err)
	}
	if n := f.Streams("sb-7"); n != 0 {
		t.Errorf("%d streams left open", n)
	}

	// The second node over the same backend adopts it untouched.
	second := node{f}
	after := second.alive(t, "sb-7")
	if after != before || !after.Alive || !after.StartedAt.Equal(start) {
		t.Errorf("after restart %+v, before %+v", after, before)
	}
	if got := screenLine(t, second.b, "sb-7", 0); got != "working" {
		t.Errorf("screen after restart = %q", got)
	}
	if err := second.b.Input(context.Background(), "sb-7", []byte("go on")); err != nil {
		t.Fatal(err)
	}
	if len(f.Launches()) != 1 {
		t.Errorf("relaunched: %d launches", len(f.Launches()))
	}
}

func TestUnknownSession(t *testing.T) {
	ctx := context.Background()
	f := termtest.New()
	for name, err := range map[string]error{
		"screen": func() error { _, err := f.Screen(ctx, "sb-x", 0); return err }(),
		"stream": func() error { _, err := f.Stream(ctx, "sb-x"); return err }(),
		"input":  f.Input(ctx, "sb-x", nil),
		"resize": f.Resize(ctx, "sb-x", 1, 1),
		"stop":   f.Stop(ctx, "sb-x", 0),
		"forget": f.Forget(ctx, "sb-x"),
		"output": f.Output("sb-x", nil),
		"exit":   f.Exit("sb-x", 1),
	} {
		if !errors.Is(err, term.ErrNoSession) {
			t.Errorf("%s: %v, want ErrNoSession", name, err)
		}
	}
}
