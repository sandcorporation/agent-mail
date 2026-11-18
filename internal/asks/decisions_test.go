package asks

import (
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

func TestRelatedTextWithOther(t *testing.T) {
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	got := relatedText([]wire.Decision{{ID: "q_1", AnsweredAt: at, Items: []wire.DecisionItem{{Header: "A", Chosen: []string{"x", "y"}, Other: "z"}, {Header: "B", Chosen: []string{}, Other: "w"}}}})
	if want := "\n\n관련 지난 결정:\n- q_1 2026-11-01: A → x, y / 기타: z; B → 기타: w"; got != want {
		t.Fatalf("%q", got)
	}
	if relatedText(nil) != "" {
		t.Fatal("empty list adds text")
	}
}
