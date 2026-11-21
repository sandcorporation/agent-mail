package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/store"
	"github.com/sandcorporation/switchboard/internal/term/termtest"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// autoMails are the system mails in role a's mailbox, oldest first, as
// "id subject|body".
func autoMails(t *testing.T, s *Server) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT id,subject,body FROM messages WHERE recipient='a' AND sender='switchboard/system' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id int64
		var subject, body string
		if err := rows.Scan(&id, &subject, &body); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d %s|%s", id, subject, body))
	}
	return out
}

var dueTimes int64

// makeDue moves the scheduled run's next time to the past, a different time
// each call (the mail is idempotent by the time).
func makeDue(t *testing.T, s *Server, id int64) {
	t.Helper()
	dueTimes++
	if _, err := s.db.Exec(`UPDATE host_autos SET next_fire_at=? WHERE id=?`, dueTimes, id); err != nil {
		t.Fatal(err)
	}
}

func sweepAutos(t *testing.T, s *Server) int {
	t.Helper()
	res, err := s.sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res.Autos
}

func lastAutoRun(t *testing.T, s *Server, id int64) wire.HostAutoRun {
	t.Helper()
	runs, err := s.hostingService().AutoRuns(id, 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs of %d: %v %v", id, runs, err)
	}
	return runs[0]
}

func TestHostAutoLaunchesAStoppedSessionAndMailsItsRole(t *testing.T) {
	s, base, cookie := signedInServer(t, true)
	token := nodeToken(t, s, "pc")
	got := adminCall(t, base, "POST", "/api/admin/terminals", cookie, true, `{"node":"pc","base":"a","profile":"team-a","directory":"/work/app","desired":"stopped"}`)
	var rec wire.HostedSession
	if err := json.Unmarshal([]byte(got.body), &rec); err != nil || got.code != http.StatusCreated {
		t.Fatalf("create = %d %s", got.code, got.body)
	}
	fake := termtest.New()
	stop := attachNode(t, base, token, fake)
	eventually(t, "the node reports the stopped record", func() bool { return terminals(t, base, cookie)[rec.ID].Runtime.State == "stopped" })

	got = adminCall(t, base, "POST", "/api/admin/terminals/"+rec.ID+"/autos", cookie, true, `{"cron":"0 9 * * *","prompt":"look for new reports","label":"reports"}`)
	var auto wire.HostAuto
	if err := json.Unmarshal([]byte(got.body), &auto); err != nil || got.code != http.StatusCreated {
		t.Fatalf("auto add = %d %s", got.code, got.body)
	}
	if auto.State != wire.HostAutoActive || auto.NextFireAt == nil || *auto.NextFireAt <= s.now().UnixMilli() || auto.TZ == "" {
		t.Fatalf("auto = %+v", auto)
	}
	if n := sweepAutos(t, s); n != 0 {
		t.Fatalf("a sweep before the time handled %d", n)
	}

	// Its time comes: the stopped session starts in a new conversation and
	// the prompt goes to its role from the system, with the label as subject.
	makeDue(t, s, auto.ID)
	if n := sweepAutos(t, s); n != 1 {
		t.Fatalf("due sweep handled %d", n)
	}
	run := lastAutoRun(t, s, auto.ID)
	after, _ := s.hostingService().Get(rec.ID)
	if run.Result != wire.AutoFired || !run.Launched || run.MailID == nil || run.AgentSessionID != after.AgentSessionID || after.AgentSessionID == rec.AgentSessionID {
		t.Fatalf("run %+v, record now %s (was %s)", run, after.AgentSessionID, rec.AgentSessionID)
	}
	eventually(t, "launched in the new conversation", func() bool {
		l := launchArgs(fake)
		return len(l) == 1 && l[0] == "sb-"+rec.ID+" --session-id "+after.AgentSessionID
	})
	if m := autoMails(t, s); len(m) != 1 || m[0] != fmt.Sprintf("%d reports|look for new reports", *run.MailID) {
		t.Fatalf("mails %q", m)
	}
	if a, _ := s.hostingService().GetAuto(auto.ID); a.NextFireAt == nil || *a.NextFireAt <= s.now().UnixMilli() {
		t.Fatalf("next time not moved on: %+v", a)
	}
	if n := sweepAutos(t, s); n != 0 {
		t.Fatalf("a second sweep handled %d", n)
	}

	// The role completes the run's mail: the session the run launched stops.
	eventually(t, "running", func() bool { return terminals(t, base, cookie)[rec.ID].Runtime.State == "running" })
	completeMail(t, base, *run.MailID)
	sweepAutos(t, s)
	if r, _ := s.hostingService().Get(rec.ID); r.Desired != "stopped" {
		t.Fatalf("after the mail was completed desired = %s", r.Desired)
	}
	if run = lastAutoRun(t, s, auto.ID); run.Result != wire.AutoFired || run.EndedAt == nil || run.Detail != "mail completed, session stopped" {
		t.Fatalf("ended run %+v", run)
	}
	eventually(t, "stopped by the completed mail", func() bool { return terminals(t, base, cookie)[rec.ID].Runtime.State == "stopped" })

	// A session someone started already runs: only the mail, and completing
	// it leaves the session running.
	if got := adminCall(t, base, "PUT", "/api/admin/terminals/"+rec.ID, cookie, true, `{"desired":"running"}`); got.code != http.StatusOK {
		t.Fatalf("start = %d %s", got.code, got.body)
	}
	eventually(t, "running", func() bool { return terminals(t, base, cookie)[rec.ID].Runtime.State == "running" })
	launches := len(launchArgs(fake))
	makeDue(t, s, auto.ID)
	sweepAutos(t, s)
	if run = lastAutoRun(t, s, auto.ID); run.Result != wire.AutoFired || run.Launched || run.MailID == nil {
		t.Fatalf("run on a running session %+v", run)
	}
	if l := launchArgs(fake); len(l) != launches {
		t.Fatalf("launches %q", l)
	}
	if m := autoMails(t, s); len(m) != 2 {
		t.Fatalf("mails %q", m)
	}
	completeMail(t, base, *run.MailID)
	sweepAutos(t, s)
	if r, _ := s.hostingService().Get(rec.ID); r.Desired != "running" {
		t.Fatalf("a session the run did not launch was stopped: %s", r.Desired)
	}

	// The node is away: node-offline, no mail, and the next time stays ahead
	// (a missed stretch is not made up).
	stop()
	eventually(t, "node offline", func() bool { return terminals(t, base, cookie)[rec.ID].Runtime.State == "node offline" })
	if _, err := s.db.Exec(`UPDATE host_autos SET next_fire_at=? WHERE id=?`, s.now().Add(-72*60*60*1e9).UnixMilli(), auto.ID); err != nil {
		t.Fatal(err)
	}
	if n := sweepAutos(t, s); n != 1 {
		t.Fatalf("offline sweep handled %d", n)
	}
	if run = lastAutoRun(t, s, auto.ID); run.Result != wire.AutoNodeOffline || run.MailID != nil {
		t.Fatalf("offline run %+v", run)
	}
	if n := sweepAutos(t, s); n != 0 {
		t.Fatalf("missed times made up: %d", n)
	}
	if m := autoMails(t, s); len(m) != 2 {
		t.Fatalf("mails while offline %q", m)
	}

	// Back at the next time.
	attachNode(t, base, token, fake)
	eventually(t, "node back", func() bool { return terminals(t, base, cookie)[rec.ID].Runtime.State == "running" })
	makeDue(t, s, auto.ID)
	sweepAutos(t, s)
	if run = lastAutoRun(t, s, auto.ID); run.Result != wire.AutoFired {
		t.Fatalf("run after the node came back %+v", run)
	}

	// --resume: a stopped session continues its last conversation.
	if got := adminCall(t, base, "PUT", "/api/admin/terminals/"+rec.ID, cookie, true, `{"desired":"stopped"}`); got.code != http.StatusOK {
		t.Fatalf("stop = %d %s", got.code, got.body)
	}
	eventually(t, "stopped", func() bool { return terminals(t, base, cookie)[rec.ID].Runtime.State == "stopped" })
	got = adminCall(t, base, "POST", "/api/admin/terminals/"+rec.ID+"/autos", cookie, true, `{"cron":"0 9 * * *","prompt":"carry on","resume":true}`)
	var resume wire.HostAuto
	if err := json.Unmarshal([]byte(got.body), &resume); err != nil || got.code != http.StatusCreated {
		t.Fatalf("auto add = %d %s", got.code, got.body)
	}
	before := len(launchArgs(fake))
	got = adminCall(t, base, "POST", fmt.Sprintf("/api/admin/terminals/%s/autos/%d/run", rec.ID, resume.ID), cookie, true, "")
	if err := json.Unmarshal([]byte(got.body), &run); err != nil || got.code != http.StatusOK || run.Result != wire.AutoFired || !run.Manual || !run.Launched {
		t.Fatalf("run now = %d %s", got.code, got.body)
	}
	eventually(t, "resumed", func() bool {
		l := launchArgs(fake)
		return len(l) == before+1 && l[before] == "sb-"+rec.ID+" --resume "+after.AgentSessionID
	})
	if m := autoMails(t, s); len(m) != 4 || !strings.HasSuffix(m[3], fmt.Sprintf("scheduled run %d|carry on", resume.ID)) {
		t.Fatalf("mails %q", m)
	}

	// Show, pause, resume and remove through the API.
	got = adminCall(t, base, "GET", fmt.Sprintf("/api/admin/terminals/%s/autos/%d?runs=2", rec.ID, auto.ID), cookie, false, "")
	var view wire.HostAutoView
	if err := json.Unmarshal([]byte(got.body), &view); err != nil || len(view.Runs) != 2 || view.Runs[0].Result != wire.AutoFired || view.Runs[1].Result != wire.AutoNodeOffline {
		t.Fatalf("show = %d %s", got.code, got.body)
	}
	got = adminCall(t, base, "POST", fmt.Sprintf("/api/admin/terminals/%s/autos/%d/pause", rec.ID, auto.ID), cookie, true, "")
	if !strings.Contains(got.body, `"state":"paused"`) {
		t.Fatalf("pause = %d %s", got.code, got.body)
	}
	makeDue(t, s, auto.ID)
	if n := sweepAutos(t, s); n != 0 {
		t.Fatalf("a paused run fired: %d", n)
	}
	got = adminCall(t, base, "POST", fmt.Sprintf("/api/admin/terminals/%s/autos/%d/resume", rec.ID, auto.ID), cookie, true, "")
	if !strings.Contains(got.body, `"state":"active"`) {
		t.Fatalf("resume = %d %s", got.code, got.body)
	}
	if got = adminCall(t, base, "GET", fmt.Sprintf("/api/admin/terminals/hs_00000000000000000000000000000000/autos/%d", auto.ID), cookie, false, ""); got.code != http.StatusNotFound {
		t.Errorf("auto under another session = %d", got.code)
	}
	if got = adminCall(t, base, "POST", "/api/admin/terminals/"+rec.ID+"/autos", cookie, true, `{"cron":"61 * * * *","prompt":"x"}`); got.code != http.StatusBadRequest {
		t.Errorf("bad cron = %d %s", got.code, got.body)
	}
	if got = adminCall(t, base, "POST", "/api/admin/terminals/"+rec.ID+"/autos", cookie, true, `{"cron":"* * * * *","prompt":"x","notify":["nobody"]}`); got.code != http.StatusBadRequest {
		t.Errorf("unknown notify role = %d %s", got.code, got.body)
	}
	if got = adminCall(t, base, "DELETE", fmt.Sprintf("/api/admin/terminals/%s/autos/%d", rec.ID, auto.ID), cookie, true, ""); got.code != http.StatusOK {
		t.Fatalf("remove = %d %s", got.code, got.body)
	}

	// A role token does not reach the admin API.
	role := map[string]string{"Authorization": "Bearer secret-a", "X-Switchboard-Role": "a"}
	for _, p := range []struct{ method, path string }{
		{"GET", "/api/admin/terminals/" + rec.ID + "/autos"},
		{"POST", "/api/admin/terminals/" + rec.ID + "/autos"},
		{"POST", fmt.Sprintf("/api/admin/terminals/%s/autos/%d/run", rec.ID, resume.ID)},
	} {
		if got := send(t, base, p.method, p.path, nil, role); got.code != http.StatusUnauthorized {
			t.Errorf("%s %s with a role token = %d", p.method, p.path, got.code)
		}
	}

	// Removing the record removes its scheduled runs and their history.
	if got := adminCall(t, base, "DELETE", "/api/admin/terminals/"+rec.ID, cookie, true, ""); got.code != http.StatusOK {
		t.Fatalf("remove record = %d", got.code)
	}
	var n int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM host_autos)+(SELECT COUNT(*) FROM host_auto_runs)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d scheduled run rows left (%v)", n, err)
	}
}

// completeMail marks mail id complete as its recipient, role a.
func completeMail(t *testing.T, base string, id int64) {
	t.Helper()
	got := send(t, base, "POST", fmt.Sprintf("/api/messages/%d/complete", id), nil, map[string]string{"Authorization": "Bearer secret-a", "X-Switchboard-Role": "a"})
	if got.code/100 != 2 {
		t.Fatalf("complete %d = %d %s", id, got.code, got.body)
	}
}

func TestHostAutoMaxRunAndBusy(t *testing.T) {
	s, base, cookie := signedInServer(t, true)
	token := nodeToken(t, s, "pc")
	got := adminCall(t, base, "POST", "/api/admin/terminals", cookie, true, `{"node":"pc","base":"a","profile":"team-a","directory":"/work/app","desired":"stopped"}`)
	var rec wire.HostedSession
	if err := json.Unmarshal([]byte(got.body), &rec); err != nil || got.code != http.StatusCreated {
		t.Fatalf("create = %d %s", got.code, got.body)
	}
	fake := termtest.New()
	attachNode(t, base, token, fake)
	eventually(t, "stopped", func() bool { return terminals(t, base, cookie)[rec.ID].Runtime.State == "stopped" })

	got = adminCall(t, base, "POST", "/api/admin/terminals/"+rec.ID+"/autos", cookie, true, `{"cron":"0 9 * * *","prompt":"p"}`)
	var plain wire.HostAuto
	if err := json.Unmarshal([]byte(got.body), &plain); err != nil || plain.MaxRunMS != wire.DefaultAutoMaxRunMS {
		t.Fatalf("default max-run: %d %s", got.code, got.body)
	}
	if got = adminCall(t, base, "POST", "/api/admin/terminals/"+rec.ID+"/autos", cookie, true, `{"cron":"0 9 * * *","prompt":"p","max_run_ms":5}`); got.code != http.StatusBadRequest {
		t.Errorf("max-run under a minute = %d %s", got.code, got.body)
	}
	got = adminCall(t, base, "POST", "/api/admin/terminals/"+rec.ID+"/autos", cookie, true, `{"cron":"0 9 * * *","prompt":"q","max_run_ms":600000}`)
	var auto wire.HostAuto
	if err := json.Unmarshal([]byte(got.body), &auto); err != nil || auto.MaxRunMS != 600000 {
		t.Fatalf("auto add = %d %s", got.code, got.body)
	}

	makeDue(t, s, auto.ID)
	sweepAutos(t, s)
	first := lastAutoRun(t, s, auto.ID)
	if first.Result != wire.AutoFired || !first.Launched || first.LaunchedAt == nil || first.EndedAt != nil {
		t.Fatalf("first run %+v", first)
	}

	// The next time comes while that session runs: busy, no mail, no launch,
	// for this scheduled run and any other of the same session.
	mails, launches := len(autoMails(t, s)), len(launchArgs(fake))
	makeDue(t, s, auto.ID)
	makeDue(t, s, plain.ID)
	sweepAutos(t, s)
	for _, id := range []int64{auto.ID, plain.ID} {
		if r := lastAutoRun(t, s, id); r.Result != wire.AutoBusy || r.MailID != nil || r.Launched {
			t.Fatalf("overlapping run of %d %+v", id, r)
		}
	}
	if len(autoMails(t, s)) != mails || len(launchArgs(fake)) != launches {
		t.Fatalf("a busy run mailed or launched")
	}

	// Max-run passes from the launch: stopped, overrun.
	if _, err := s.db.Exec(`UPDATE host_auto_runs SET launched_at=? WHERE id=?`, *first.LaunchedAt-600000, first.ID); err != nil {
		t.Fatal(err)
	}
	sweepAutos(t, s)
	runs, _ := s.hostingService().AutoRuns(auto.ID, 5)
	var over wire.HostAutoRun
	for _, r := range runs {
		if r.ID == first.ID {
			over = r
		}
	}
	if over.Result != wire.AutoOverrun || over.EndedAt == nil || !strings.Contains(over.Detail, "max-run 10m0s") {
		t.Fatalf("overrun run %+v", over)
	}
	if r, _ := s.hostingService().Get(rec.ID); r.Desired != "stopped" {
		t.Fatalf("after max-run desired = %s", r.Desired)
	}

	// Nothing holds the session now: the next time launches it again, in a
	// new conversation.
	makeDue(t, s, auto.ID)
	sweepAutos(t, s)
	if r := lastAutoRun(t, s, auto.ID); r.Result != wire.AutoFired || !r.Launched || r.AgentSessionID == first.AgentSessionID {
		t.Fatalf("run after overrun %+v", r)
	}

	// Stopped by hand: no longer held, so the next run is not busy.
	if got := adminCall(t, base, "PUT", "/api/admin/terminals/"+rec.ID, cookie, true, `{"desired":"stopped"}`); got.code != http.StatusOK {
		t.Fatalf("stop = %d %s", got.code, got.body)
	}
	makeDue(t, s, plain.ID)
	sweepAutos(t, s)
	if r := lastAutoRun(t, s, plain.ID); r.Result != wire.AutoFired || !r.Launched {
		t.Fatalf("run after a stop by hand %+v", r)
	}
}

func TestHostAutoKeepsTheNewest500Runs(t *testing.T) {
	s, _, _ := signedInServer(t, true)
	nodeToken(t, s, "pc")
	rec, err := s.hostingService().Create(wire.HostedSessionCreate{Node: "pc", Base: "a", Profile: "team-a", Directory: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.hostingService().CreateAuto(rec.ID, wire.HostAutoCreate{Cron: "* * * * *", Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<502) INSERT INTO host_auto_runs(auto,at,result) SELECT ?,i,'node-offline' FROM n`, a.ID); err != nil {
		t.Fatal(err)
	}
	makeDue(t, s, a.ID)
	sweepAutos(t, s) // the node is not attached: one more node-offline line
	var n int
	var oldest int64
	if err := s.db.QueryRow(`SELECT COUNT(*),MIN(id) FROM host_auto_runs WHERE auto=?`, a.ID).Scan(&n, &oldest); err != nil || n != 500 || oldest != 4 {
		t.Fatalf("%d runs kept, oldest %d (%v)", n, oldest, err)
	}
}

func TestHostAutoCLI(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pc.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO roles(role,token,created_at) VALUES('team/worker','h','now')`,
		`INSERT INTO roles(role,token,created_at) VALUES('team/boss','h2','now')`,
		`INSERT INTO nodes(name,token_hash,created_at) VALUES('pc','h',1)`,
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	code, id, stderr := hostCLI(t, dbPath, "add", "--node", "pc", "--base", "team/worker", "--profile", "team-worker", "--dir", "/work/app")
	if code != 0 {
		t.Fatalf("host add = %d %s", code, stderr)
	}

	code, out, stderr := hostCLI(t, dbPath, "auto", "add", id, "--cron", "*/5 * * * *", "--prompt", "check the queue", "--label", "queue", "--resume", "--notify", "team/boss", "--max-run", "30m", "--json")
	var a wire.HostAuto
	if code != 0 || json.Unmarshal([]byte(out), &a) != nil || a.Session != id || !a.Resume || a.Label != "queue" || len(a.Notify) != 1 || a.Notify[0] != "team/boss" || a.NextFireAt == nil || a.MaxRunMS != 30*60*1000 {
		t.Fatalf("auto add = %d %q %s", code, out, stderr)
	}
	aid := fmt.Sprint(a.ID)
	for _, bad := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"auto", "add", id, "--cron", "* * * * *"}, 2, "requires --cron and --prompt"},
		{[]string{"auto", "add", "hs_00000000000000000000000000000000", "--cron", "* * * * *", "--prompt", "p"}, 1, "no hosted session"},
		{[]string{"auto", "add", id, "--cron", "nope", "--prompt", "p"}, 1, "cron"},
		{[]string{"auto", "add", id, "--cron", "* * * * *", "--prompt", "p", "--notify", "team/nobody"}, 1, "not a registered role"},
		{[]string{"auto", "add", id, "--cron", "* * * * *", "--prompt", "p", "--check-timeout", "30s"}, 2, "--check-timeout needs --check"},
		{[]string{"auto", "add", id, "--cron", "* * * * *", "--prompt", "p", "--check", "true", "--check-timeout", "11m"}, 2, "1s to 10m"},
		{[]string{"auto", "show", aid, "--check", "true"}, 2, "--check is not one of its options"},
		{[]string{"auto", "show", aid, "--cron", "* * * * *"}, 2, "--cron is not one of its options"},
		{[]string{"auto", "add", id, "--cron", "* * * * *", "--prompt", "p", "--max-run", "10s"}, 2, "--max-run wants a duration"},
		{[]string{"auto", "show", aid, "--max-run", "1h"}, 2, "--max-run is not one of its options"},
		{[]string{"auto", "show", "999"}, 1, "no scheduled run 999"},
		{[]string{"auto", "fly", aid}, 2, "unknown action"},
		{[]string{"auto"}, 2, "requires ACTION"},
		{[]string{"list", "--cron", "* * * * *"}, 2, "is for host auto"},
		{[]string{"start", id, "extra"}, 2, "unexpected argument"},
	} {
		if code, _, stderr := hostCLI(t, dbPath, bad.args...); code != bad.code || !strings.Contains(stderr, bad.want) {
			t.Errorf("%q = %d %s, want %d %q", bad.args, code, stderr, bad.code, bad.want)
		}
	}

	if code, out, stderr = hostCLI(t, dbPath, "auto", "list"); code != 0 || !strings.Contains(out, "*/5 * * * *") || !strings.Contains(out, "queue") || !strings.Contains(out, id) {
		t.Fatalf("auto list = %d %q %s", code, out, stderr)
	}
	if code, out, _ = hostCLI(t, dbPath, "auto", "run", aid); code != 0 || out != aid+" run requested (the host runs it on its next sweep)" {
		t.Fatalf("auto run = %d %q", code, out)
	}
	if code, out, _ = hostCLI(t, dbPath, "auto", "pause", aid); code != 0 || !strings.HasPrefix(out, aid+" paused, next -") {
		t.Fatalf("auto pause = %d %q", code, out)
	}
	if code, out, _ = hostCLI(t, dbPath, "auto", "resume", aid); code != 0 || !strings.HasPrefix(out, aid+" active, next 2") {
		t.Fatalf("auto resume = %d %q", code, out)
	}
	code, out, _ = hostCLI(t, dbPath, "auto", "show", aid, "--runs", "5", "--json")
	var v wire.HostAutoView
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil || v.RunRequestedAt == nil || len(v.Runs) != 0 || v.Prompt != "check the queue" {
		t.Fatalf("auto show = %d %q", code, out)
	}
	if code, out, _ = hostCLI(t, dbPath, "auto", "show", aid); code != 0 || !strings.Contains(out, "launch: resume, max-run 30m0s") || !strings.Contains(out, "no runs yet") {
		t.Fatalf("auto show text = %d %q", code, out)
	}
	code, out, stderr = hostCLI(t, dbPath, "auto", "add", id, "--cron", "0 * * * *", "--prompt", "p", "--check", "test -s queue.txt", "--check-timeout", "2m", "--json")
	var checked wire.HostAuto
	if code != 0 || json.Unmarshal([]byte(out), &checked) != nil || checked.Check != "test -s queue.txt" || checked.CheckTimeoutMS != 120_000 {
		t.Fatalf("auto add --check = %d %q %s", code, out, stderr)
	}
	if code, out, _ = hostCLI(t, dbPath, "auto", "show", fmt.Sprint(checked.ID)); code != 0 || !strings.Contains(out, "check (limit 2m0s): test -s queue.txt") {
		t.Fatalf("auto show of a checked run = %d %q", code, out)
	}
	if code, _, stderr = hostCLI(t, dbPath, "auto", "rm", fmt.Sprint(checked.ID)); code != 0 {
		t.Fatalf("auto rm = %d %s", code, stderr)
	}
	if code, out, _ = hostCLI(t, dbPath, "auto", "rm", aid); code != 0 || out != "removed "+aid {
		t.Fatalf("auto rm = %d %q", code, out)
	}
	if code, out, _ = hostCLI(t, dbPath, "auto", "list"); code != 0 || out != "no scheduled runs" {
		t.Fatalf("auto list after rm = %d %q", code, out)
	}
	// The old host commands keep their shape.
	if code, out, _ = hostCLI(t, dbPath, "stop", id); code != 0 || out != id+" stopped" {
		t.Fatalf("host stop = %d %q", code, out)
	}
}

// A `host auto run` written by the CLI is fired by the host's next sweep,
// once, marked manual.
func TestHostAutoRunRequestFiresOnTheNextSweep(t *testing.T) {
	s, base, cookie := signedInServer(t, true)
	token := nodeToken(t, s, "pc")
	rec, err := s.hostingService().Create(wire.HostedSessionCreate{Node: "pc", Base: "a", Profile: "team-a", Directory: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	attachNode(t, base, token, termtest.New())
	eventually(t, "running", func() bool { return terminals(t, base, cookie)[rec.ID].Runtime.State == "running" })
	a, err := s.hostingService().CreateAuto(rec.ID, wire.HostAutoCreate{Cron: "0 0 1 1 *", Prompt: "now please"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.hostingService().PauseAuto(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.hostingService().RequestAutoRun(a.ID); err != nil {
		t.Fatal(err)
	}
	if n := sweepAutos(t, s); n != 1 {
		t.Fatalf("sweep handled %d", n)
	}
	if n := sweepAutos(t, s); n != 0 {
		t.Fatalf("second sweep handled %d", n)
	}
	if run := lastAutoRun(t, s, a.ID); run.Result != wire.AutoFired || !run.Manual || run.Launched || run.MailID == nil {
		t.Fatalf("run %+v", run)
	}
	if m := autoMails(t, s); len(m) != 1 || !strings.HasSuffix(m[0], "|now please") {
		t.Fatalf("mails %q", m)
	}
}
