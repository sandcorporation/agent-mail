package server

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/cli"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// schedule add, list, show and rm through the real CLI: the cron is recorded
// with the machine's zone, --in becomes a once schedule.
func TestScheduleCLI(t *testing.T) {
	h := newCLIHarness(t)
	h.configure(nil, "a")
	tz := []string{"TZ=Asia/Seoul"}
	r := h.run(tz, "schedule", "add", "--cron", "0 9 * * 1-5", "--label", "standup", "--prompt", "Check the build")
	if r.code != cli.ExitOK {
		t.Fatalf("add cron: %+v", r)
	}
	id, err := strconv.ParseInt(strings.TrimSpace(r.stdout), 10, 64)
	if err != nil {
		t.Fatalf("add printed %q", r.stdout)
	}
	sc, err := h.s.jobs().GetSchedule(id)
	if err != nil || sc.Kind != wire.ScheduleCron || sc.TZ != "Asia/Seoul" || sc.Cron != "0 9 * * 1-5" || sc.Prompt != "Check the build" || sc.Role != "a" {
		t.Fatalf("stored %+v %v", sc, err)
	}
	r = h.run(tz, "schedule", "add", "--in", "20m", "--prompt", "Look again", "--json")
	var once wire.Schedule
	if r.code != cli.ExitOK || json.Unmarshal([]byte(r.stdout), &once) != nil || once.Kind != wire.ScheduleOnce || once.NextFireAt == nil {
		t.Fatalf("add --in: %+v", r)
	}
	if d := time.Until(*once.NextFireAt); d < 19*time.Minute || d > 21*time.Minute {
		t.Fatalf("--in 20m fires in %s", d)
	}
	for _, bad := range [][]string{
		{"schedule", "add", "--prompt", "x"},
		{"schedule", "add", "--cron", "* * * * *", "--in", "5m", "--prompt", "x"},
		{"schedule", "add", "--cron", "* * *", "--prompt", "x"},
		{"schedule", "add", "--in", "5m"},
		{"schedule", "add", "--at", "tomorrow", "--prompt", "x"},
		{"schedule", "show"},
	} {
		if r := h.run(tz, bad...); r.code != cli.ExitUsage {
			t.Errorf("%v: %+v", bad, r)
		}
	}
	r = h.run(tz, "schedule", "list")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "#"+itoa(id)+"  active  cron \"0 9 * * 1-5\"") || !strings.Contains(r.stdout, "standup") {
		t.Fatalf("list: %+v", r)
	}
	r = h.run(tz, "schedule", "show", itoa(id))
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "(Asia/Seoul)") || !strings.Contains(r.stdout, "prompt   Check the build") {
		t.Fatalf("show: %+v", r)
	}
	if r = h.run(tz, "schedule", "rm", itoa(id)); r.code != cli.ExitOK {
		t.Fatalf("rm: %+v", r)
	}
	r = h.run(tz, "schedule", "list", "--json")
	var list []wire.Schedule
	if r.code != cli.ExitOK || json.Unmarshal([]byte(r.stdout), &list) != nil || len(list) != 1 || list[0].ID != once.ID {
		t.Fatalf("list after rm: %+v", r)
	}
}
