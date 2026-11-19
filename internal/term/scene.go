package term

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// Scene is a scripted screen: the testdata/scenes/*.vt files the golden
// tests play, and what the fake agent paints on "draw <scene>".
//
// Format: "#" lines are comments, "@size CxR" (first) and "@resize CxR" are
// directives, every other line is VT input with its line break ignored and
// the escapes \e \r \n \t \\ \xNN.
type Scene struct {
	Cols, Rows int
	Steps      []SceneStep
}

// SceneStep is VT bytes to write, or a resize when Data is nil.
type SceneStep struct {
	Data       []byte
	Cols, Rows int
}

// ParseScene reads a scene file's contents.
func ParseScene(raw []byte) (Scene, error) {
	var sc Scene
	for i, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "@size "):
			c, r, err := parseSceneSize(line[len("@size "):])
			if err != nil {
				return Scene{}, fmt.Errorf("line %d: %w", i+1, err)
			}
			sc.Cols, sc.Rows = c, r
		case strings.HasPrefix(line, "@resize "):
			c, r, err := parseSceneSize(line[len("@resize "):])
			if err != nil {
				return Scene{}, fmt.Errorf("line %d: %w", i+1, err)
			}
			sc.Steps = append(sc.Steps, SceneStep{Cols: c, Rows: r})
		default:
			b, err := unescapeScene(line)
			if err != nil {
				return Scene{}, fmt.Errorf("line %d: %w", i+1, err)
			}
			sc.Steps = append(sc.Steps, SceneStep{Data: b})
		}
	}
	if sc.Cols == 0 {
		return Scene{}, fmt.Errorf("no @size")
	}
	return sc, nil
}

func parseSceneSize(s string) (int, int, error) {
	c, r, ok := strings.Cut(strings.TrimSpace(s), "x")
	cols, err1 := strconv.Atoi(c)
	rows, err2 := strconv.Atoi(r)
	if !ok || err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("bad size %q", s)
	}
	return cols, rows, nil
}

func unescapeScene(s string) ([]byte, error) {
	var b bytes.Buffer
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'e':
			b.WriteByte(0x1b)
		case 'r':
			b.WriteByte('\r')
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case '\\':
			b.WriteByte('\\')
		case 'x':
			if i+3 > len(s) {
				return nil, fmt.Errorf("bad \\x escape in %q", s)
			}
			n, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				return nil, fmt.Errorf("bad \\x escape in %q", s)
			}
			b.WriteByte(byte(n))
			i += 2
		default:
			return nil, fmt.Errorf("unknown escape \\%c in %q", s[i], s)
		}
	}
	return b.Bytes(), nil
}
