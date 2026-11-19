package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	for _, tc := range []struct {
		in     string
		major  int
		minor  int
		dev    bool
		accept bool
	}{
		{"tmux 3.2", 3, 2, false, true},
		{"tmux 3.2a\n", 3, 2, false, true},
		{"tmux 3.3a", 3, 3, false, true},
		{"tmux 3.6", 3, 6, false, true},
		{"tmux next-3.4", 3, 4, false, true},
		{"tmux 3.2-rc2", 3, 2, false, true},
		{"tmux 4.0", 4, 0, false, true},
		{"tmux 10.1", 10, 1, false, true},
		{"tmux master", 0, 0, true, true},
		{"tmux 3.1c", 3, 1, false, false},
		{"tmux 3.0a", 3, 0, false, false},
		{"tmux 2.9", 2, 9, false, false},
		{"tmux 1.8", 1, 8, false, false},
		{"tmux next-3.1", 3, 1, false, false},
	} {
		v, err := ParseVersion(tc.in)
		if err != nil {
			t.Errorf("ParseVersion(%q): %v", tc.in, err)
			continue
		}
		if v.Major != tc.major || v.Minor != tc.minor || v.Dev != tc.dev {
			t.Errorf("ParseVersion(%q) = %+v", tc.in, v)
		}
		if got := v.AtLeast(MinVersion); got != tc.accept {
			t.Errorf("ParseVersion(%q).AtLeast(3.2) = %v, want %v", tc.in, got, tc.accept)
		}
	}
	for _, bad := range []string{"", "tmux", "tmux three", "screen 4.9"} {
		if v, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) = %+v, want error", bad, v)
		}
	}
}

func TestCheckMissingBinary(t *testing.T) {
	_, err := Check(context.Background(), "switchboard-no-such-tmux")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	for _, want := range []string{"not found", "apt-get install tmux", "dnf install tmux"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}
