package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

func TestStatusTableSinceColumnLast(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	busy, reported := now.Add(-(7*time.Hour + 12*time.Minute)), now.Add(-4*time.Second)
	out := wire.PresenceList{Now: now, Sessions: []wire.PresenceView{
		{Role: "team/worker-1", Status: "busy", Machine: "m", Directory: "/a", ReportedAt: &reported, Since: &busy},
		{Role: "team/worker-2", Status: "idle", Machine: "m", Directory: "/b"},
	}}
	var b strings.Builder
	if err := writeStatusTable(&b, out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	want := []string{"ROLE", "STATUS", "CONTEXT", "LAST", "TURN", "MACHINE", "DIRECTORY", "LABEL", "PARENT", "REPORTED", "SINCE"}
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("header %q", lines[0])
	}
	for i, wantSince := range []string{"7h12m", "-"} {
		f := strings.Fields(lines[i+1])
		if f[0] != out.Sessions[i].Role || f[len(f)-1] != wantSince {
			t.Errorf("row %d: %q, want SINCE %q at the end", i, lines[i+1], wantSince)
		}
	}
	if !strings.Contains(lines[1], "4s ago") {
		t.Errorf("REPORTED moved: %q", lines[1])
	}
}
