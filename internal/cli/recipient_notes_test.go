package cli

import (
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// The lines a send prints about the receiving mailbox (#354).
func TestRecipientNotes(t *testing.T) {
	cases := []struct {
		name string
		m    wire.Message
		want string
	}{
		{"live", wire.Message{NoSession: true, RecipientState: &wire.RecipientState{Mailbox: "team/worker", Live: true}}, ""},
		{"ended and held", wire.Message{RecipientState: &wire.RecipientState{Mailbox: "team/worker-2", EndedS: 7*60 + 30, Held: true}},
			"받는 세션 없음: team/worker-2, 마지막 세션 7분 전 끝남. 붙잡아 둠"},
		{"slot 1 empty, others live", wire.Message{NoSession: false, RecipientState: &wire.RecipientState{Mailbox: "team/worker", LiveSlots: []string{"team/worker-2", "team/worker-3"}}},
			"team/worker-2, team/worker-3 이 살아 있음. 그 슬롯 이름으로 보내라"},
		{"held, others live", wire.Message{RecipientState: &wire.RecipientState{Mailbox: "team/worker", EndedS: 60, Held: true, LiveSlots: []string{"team/worker-2"}}},
			"받는 세션 없음: team/worker, 마지막 세션 1분 전 끝남. 붙잡아 둠|team/worker-2 이 살아 있음. 그 슬롯 이름으로 보내라"},
		{"never attached", wire.Message{NoSession: true, RecipientState: &wire.RecipientState{Mailbox: "team/worker"}},
			"no session of team/worker is attached; the mail waits in its inbox"},
		{"server before #354", wire.Message{NoSession: true}, "no session of team/worker is attached; the mail waits in its inbox"},
	}
	for _, c := range cases {
		if got := strings.Join(RecipientNotes("team/worker", c.m), "|"); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
