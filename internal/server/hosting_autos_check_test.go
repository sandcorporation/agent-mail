package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/node"
	"github.com/sandcorporation/switchboard/internal/term/termtest"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// checkedRun waits for the scheduled run id's history to reach n lines and
// returns the newest.
func checkedRun(t *testing.T, s *Server, id int64, n int) wire.HostAutoRun {
	t.Helper()
	var runs []wire.HostAutoRun
	eventually(t, fmt.Sprintf("run %d of scheduled run %d recorded", n, id), func() bool {
		var err error
		runs, err = s.hostingService().AutoRuns(id, 0)
		return err == nil && len(runs) >= n
	})
	return runs[0]
}

func addCheckedAuto(t *testing.T, base string, cookie *http.Cookie, rec, body string) wire.HostAuto {
	t.Helper()
	got := adminCall(t, base, "POST", "/api/admin/terminals/"+rec+"/autos", cookie, true, body)
	var a wire.HostAuto
	if err := json.Unmarshal([]byte(got.body), &a); err != nil || got.code != http.StatusCreated {
		t.Fatalf("auto add %s = %d %s", body, got.code, got.body)
	}
	return a
}

func checkBody(cmd string, extra string) string {
	b, _ := json.Marshal(cmd)
	return `{"cron":"0 9 * * *","prompt":"look for new reports","check":` + string(b) + extra + `}`
}

// The precheck's exit decides the run: 0 fires with the output under the
// prompt, 1 skips (no session, no mail), anything else and the limit are
// recorded; a node without --allow-checks refuses (#334).
func TestHostAutoPrecheck(t *testing.T) {
	t.Parallel()
	s, base, cookie := signedInServer(t, true)
	token := nodeToken(t, s, "pc")
	dir, _ := json.Marshal(t.TempDir())
	got := adminCall(t, base, "POST", "/api/admin/terminals", cookie, true, `{"node":"pc","base":"a","profile":"team-a","directory":`+string(dir)+`,"desired":"stopped"}`)
	var rec wire.HostedSession
	if err := json.Unmarshal([]byte(got.body), &rec); err != nil || got.code != http.StatusCreated {
		t.Fatalf("create = %d %s", got.code, got.body)
	}
	for _, bad := range []string{
		`{"cron":"0 9 * * *","prompt":"p","check_timeout_ms":5000}`,
		checkBody("exit 0", `,"check_timeout_ms":600001`),
		checkBody("exit 0", `,"check_timeout_ms":999`),
	} {
		if got := adminCall(t, base, "POST", "/api/admin/terminals/"+rec.ID+"/autos", cookie, true, bad); got.code != http.StatusBadRequest {
			t.Fatalf("auto add %s = %d %s", bad, got.code, got.body)
		}
	}
	fake := termtest.New()

	// Checks off on the node: refused, recorded as error.
	stop := attachNode(t, base, token, fake)
	eventually(t, "the node reports the record", func() bool { return terminals(t, base, cookie)[rec.ID].Runtime.State == "stopped" })
	ok := addCheckedAuto(t, base, cookie, rec.ID, checkBody("echo ready", ""))
	if ok.Check != "echo ready" || ok.CheckTimeoutMS != wire.DefaultCheckTimeoutMS {
		t.Fatalf("auto %+v", ok)
	}
	makeDue(t, s, ok.ID)
	sweepAutos(t, s)
	if run := checkedRun(t, s, ok.ID, 1); run.Result != wire.AutoError || !strings.Contains(run.Detail, node.ErrChecksOff) || run.MailID != nil || run.Launched {
		t.Fatalf("refused run %+v", run)
	}
	stop()
	attachNodeWith(t, base, token, fake, node.Config{AllowChecks: true, Environ: os.Environ()})
	eventually(t, "the node is back", func() bool { return terminals(t, base, cookie)[rec.ID].Runtime.State == "stopped" })

	skip := addCheckedAuto(t, base, cookie, rec.ID, checkBody("echo nothing new && exit 1", ""))
	makeDue(t, s, skip.ID)
	sweepAutos(t, s)
	run := checkedRun(t, s, skip.ID, 1)
	if run.Result != wire.AutoSkipped || run.ExitCode == nil || *run.ExitCode != 1 || !strings.Contains(run.Output, "nothing new") || run.MailID != nil || run.Launched || run.DurationMS == nil {
		t.Fatalf("skipped run %+v", run)
	}

	other := addCheckedAuto(t, base, cookie, rec.ID, checkBody("exit 3", ""))
	makeDue(t, s, other.ID)
	sweepAutos(t, s)
	if run = checkedRun(t, s, other.ID, 1); run.Result != wire.AutoError || run.ExitCode == nil || *run.ExitCode != 3 || run.Launched {
		t.Fatalf("exit 3 run %+v", run)
	}

	sleep := "sleep 30"
	if runtime.GOOS == "windows" {
		sleep = "ping -n 30 127.0.0.1 >nul"
	}
	slow := addCheckedAuto(t, base, cookie, rec.ID, checkBody(sleep, `,"check_timeout_ms":1000`))
	makeDue(t, s, slow.ID)
	sweepAutos(t, s)
	if run = checkedRun(t, s, slow.ID, 1); run.Result != wire.AutoTimeout || run.ExitCode != nil || run.Launched || run.MailID != nil {
		t.Fatalf("timed-out run %+v", run)
	}
	if l := launchArgs(fake); len(l) != 0 || len(autoMails(t, s)) != 0 {
		t.Fatalf("launches %q, mails %q before any run fired", l, autoMails(t, s))
	}

	// Exit 0: the T1 path, with the output under the prompt.
	makeDue(t, s, ok.ID)
	sweepAutos(t, s)
	run = checkedRun(t, s, ok.ID, 2)
	if run.Result != wire.AutoFired || !run.Launched || run.MailID == nil || run.ExitCode == nil || *run.ExitCode != 0 {
		t.Fatalf("fired run %+v", run)
	}
	if m := autoMails(t, s); len(m) != 1 || m[0] != fmt.Sprintf("%d scheduled run %d|look for new reports\n\nready", *run.MailID, ok.ID) {
		t.Fatalf("mails %q", m)
	}
}
