package mail

import (
	"fmt"
	"testing"
)

func TestMentions(t *testing.T) {
	for text, want := range map[string]string{
		"Re #499·#500":         "[499 500]",
		"Re #499, #500 done":   "[499 500]",
		"#4990 and #499a":      "[4990]",
		"(#499).":              "[499]",
		"#499은 끝":              "[499]",
		"no ids # 12, x#7_":    "[]",
		"issue #0 and #12\n#3": "[12 3]",
	} {
		if got := fmt.Sprint(Mentions(text)); got != want {
			t.Errorf("Mentions(%q) = %s, want %s", text, got, want)
		}
	}
}
