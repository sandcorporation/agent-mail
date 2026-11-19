package term

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/scenes/*.golden from the emulator")

type scene struct{ Scene }

func parseScene(t *testing.T, path string) scene {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := ParseScene(raw)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return scene{sc}
}

// play feeds a scene through a fresh emulator.
func (sc scene) play(t *testing.T) Grid {
	t.Helper()
	e := NewEmulator(sc.Cols, sc.Rows, 0)
	defer e.Close()
	for _, s := range sc.Steps {
		if s.Data == nil {
			e.Resize(s.Cols, s.Rows)
			continue
		}
		if _, err := e.Write(s.Data); err != nil {
			t.Fatal(err)
		}
	}
	return e.Grid(-1)
}

func scenePaths(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "scenes", "*.vt"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no scenes: %v", err)
	}
	return paths
}

func TestScenesMatchGolden(t *testing.T) {
	for _, path := range scenePaths(t) {
		name := strings.TrimSuffix(filepath.Base(path), ".vt")
		t.Run(name, func(t *testing.T) {
			got := parseScene(t, path).play(t).Dump()
			golden := strings.TrimSuffix(path, ".vt") + ".golden"
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("grid differs from %s\n--- got\n%s--- want\n%s", golden, got, want)
			}
		})
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	for _, path := range scenePaths(t) {
		name := strings.TrimSuffix(filepath.Base(path), ".vt")
		t.Run(name, func(t *testing.T) {
			want := parseScene(t, path).play(t)
			e := NewEmulator(want.Cols, want.Rows, 0)
			defer e.Close()
			if _, err := e.Write(want.Snapshot()); err != nil {
				t.Fatal(err)
			}
			got := e.Grid(-1)
			if got.Dump() != want.Dump() {
				t.Errorf("round trip differs\n--- got\n%s--- want\n%s", got.Dump(), want.Dump())
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("round trip cells differ")
			}
		})
	}
}

func TestGridHistoryLimit(t *testing.T) {
	e := NewEmulator(8, 2, 0)
	defer e.Close()
	if _, err := e.Write([]byte("a\r\nb\r\nc\r\nd\r\ne")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		history int
		want    []string
	}{
		{0, nil},
		{2, []string{"b", "c"}},
		{-1, []string{"a", "b", "c"}},
		{9, []string{"a", "b", "c"}},
	} {
		g := e.Grid(tc.history)
		var got []string
		for _, l := range g.History {
			got = append(got, lineText(l))
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("Grid(%d) history = %q, want %q", tc.history, got, tc.want)
		}
	}
}

func TestEmulatorIgnoresTerminalQueries(t *testing.T) {
	// Device attribute and cursor position queries make the library write a
	// reply; nothing reads replies, so they must not block the writer.
	e := NewEmulator(10, 2, 0)
	defer e.Close()
	for range 100 {
		if _, err := e.Write([]byte("\x1b[c\x1b[>c\x1b[6n\x1b[?2004$p")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if got := lineText(e.Grid(0).Screen[0]); got != "ok        " {
		t.Errorf("screen = %q", got)
	}
}
