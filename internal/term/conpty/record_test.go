package conpty_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/term"
)

// encodeScene writes recorded output in the scene format term.ParseScene
// reads: data lines split before an escape once a line is long, so no line
// breaks a UTF-8 character or an escape sequence.
func encodeScene(b *strings.Builder, data []byte) {
	line := 0
	for _, c := range data {
		if c == 0x1b && line >= 80 {
			b.WriteByte('\n')
			line = 0
		}
		var s string
		switch {
		case c == 0x1b:
			s = `\e`
		case c == '\r':
			s = `\r`
		case c == '\n':
			s = `\n`
		case c == '\t':
			s = `\t`
		case c == '\\':
			s = `\\`
		case c < 0x20 || c == 0x7f:
			s = fmt.Sprintf(`\x%02x`, c)
		default:
			s = string([]byte{c})
		}
		b.WriteString(s)
		line += len(s)
	}
	if line > 0 {
		b.WriteByte('\n')
	}
}

// recording is a scene's ConPTY output: chunks of bytes separated by the
// resizes the test made.
type recording struct {
	cols, rows int
	parts      []recordPart
}

type recordPart struct {
	data       []byte
	cols, rows int // a resize when data is nil
}

func (r recording) encode(header string) []byte {
	var b strings.Builder
	for _, l := range strings.Split(header, "\n") {
		b.WriteString("# " + l + "\n")
	}
	fmt.Fprintf(&b, "@size %dx%d\n", r.cols, r.rows)
	for _, p := range r.parts {
		if p.data == nil {
			fmt.Fprintf(&b, "@resize %dx%d\n", p.cols, p.rows)
			continue
		}
		encodeScene(&b, p.data)
	}
	return []byte(b.String())
}

func TestEncodeSceneRoundTrip(t *testing.T) {
	data := []byte("plain \\ text\x1b[1;31mred\x1b[0m\r\n\t\x07\x7f 한글 ✨" + strings.Repeat("x", 90) + "\x1b[H\x1b[2J")
	rec := recording{cols: 10, rows: 3, parts: []recordPart{{data: data}, {cols: 12, rows: 4}, {data: []byte("after")}}}
	sc, err := term.ParseScene(rec.encode("header\nsecond line"))
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	var resizes []string
	for _, s := range sc.Steps {
		if s.Data == nil {
			resizes = append(resizes, fmt.Sprintf("%dx%d", s.Cols, s.Rows))
			got = append(got, '|')
			continue
		}
		got = append(got, s.Data...)
	}
	if want := string(data) + "|after"; string(got) != want {
		t.Fatalf("round trip\n got %q\nwant %q", got, want)
	}
	if sc.Cols != 10 || sc.Rows != 3 || len(resizes) != 1 || resizes[0] != "12x4" {
		t.Fatalf("size %dx%d resizes %v", sc.Cols, sc.Rows, resizes)
	}
}

// TestRecordedConPTYOutputMatchesGolden replays the ConPTY output recorded
// on Windows for every golden scene (testdata/conpty/*.vt, written by the
// Windows golden test with -conpty.record) through the emulator, on every
// OS: the emulator side of Windows behaviour is covered on Linux CI too.
func TestRecordedConPTYOutputMatchesGolden(t *testing.T) {
	scenes, err := filepath.Glob(filepath.Join("..", "testdata", "scenes", "*.vt"))
	if err != nil || len(scenes) == 0 {
		t.Fatalf("no scenes: %v", err)
	}
	for _, scene := range scenes {
		name := filepath.Base(scene)
		t.Run(strings.TrimSuffix(name, ".vt"), func(t *testing.T) {
			rec := filepath.Join("..", "testdata", "conpty", name)
			if _, err := os.Stat(rec); err != nil {
				t.Fatalf("no ConPTY recording for %s: %v", name, err)
			}
			// The emulator matches the goldens (term's own scene test), so
			// the scene played here stands for its golden.
			got := conptyView(play(t, readScene(t, rec)))
			want := conptyView(play(t, readScene(t, scene)))
			if got != want {
				t.Errorf("recorded ConPTY output differs from golden\n--- got\n%s--- want\n%s", got, want)
			}
		})
	}
}

// conptyView dumps g as ConPTY is held to it. ConPTY leaves the cursor where
// it was while the cursor is hidden, and paints a blank cell with whatever
// foreground and weight are current; neither shows on screen, so both are
// dropped here.
func conptyView(g term.Grid) string {
	if g.Cursor.Hidden {
		g.Cursor.X, g.Cursor.Y = 0, 0
	}
	g.History, g.Screen = blankStyles(g.History), blankStyles(g.Screen)
	return g.Dump()
}

func blankStyles(rows [][]term.Cell) [][]term.Cell {
	out := make([][]term.Cell, len(rows))
	for i, row := range rows {
		out[i] = append([]term.Cell(nil), row...)
		for j, c := range out[i] {
			a := c.Attr
			if strings.TrimSpace(c.Text) != "" || a.Reverse || a.Underline || a.Strike {
				continue
			}
			out[i][j].Attr = term.Attr{Bg: a.Bg}
		}
	}
	return out
}
