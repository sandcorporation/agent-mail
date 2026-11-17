package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const progressJSON = `{"generated_at":"2026-11-17T12:00:00Z","pending_alert_s":14400,"services":[` +
	`{"service":"web","badge":"busy","sessions":[` +
	`{"role":"web/coord","state":"busy","since":"2026-11-17T11:00:00Z","directory":"/home/u/workspace/web","last_seen":"2026-11-17T11:59:50Z"},` +
	`{"role":"web/worker-1","state":"error:api","since":"2026-11-17T11:30:00Z","directory":"","last_seen":"2026-11-17T11:59:50Z"}],` +
	`"last_report":{"id":7,"from":"web/coord","to":"team/boss","subject":"PR 열림","at":"2026-11-17T11:48:00Z"},` +
	`"last_activity_at":"2026-11-17T11:50:00Z",` +
	`"open_directives":[{"id":5,"to":"web/coord","subject":"배포 준비","status":"read","age_s":18000,"over_threshold":true},` +
	`{"id":6,"to":"web/coord","subject":"","status":"pending","age_s":600,"over_threshold":false}],` +
	`"open_tasks":[{"task_id":"web-12","last_subject":"시험 추가","last_at":"2026-11-15T12:00:00Z","messages":3}]},` +
	`{"service":"api","badge":"idle","sessions":[{"role":"api","state":"idle","since":"2026-11-17T10:00:00Z","directory":"srv","last_seen":"2026-11-17T11:59:00Z"}],` +
	`"last_report":null,"last_activity_at":"2026-11-17T11:59:30Z","open_directives":[],"open_tasks":[]},` +
	`{"service":"old","badge":"none","sessions":[],"last_report":null,"last_activity_at":null,"open_directives":[],"open_tasks":[]}` +
	`]}`

// fakeProgressAPI stands in for GET /api/progress: it records each request and
// answers status, or the canned JSON when status is 0.
type fakeProgressAPI struct {
	mu     sync.Mutex
	status int
	reqs   []*http.Request
}

func (f *fakeProgressAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	status := f.status
	f.mu.Unlock()
	if r.URL.Path != "/api/progress" || r.Method != "GET" {
		http.NotFound(w, r)
		return
	}
	if status != 0 {
		http.Error(w, "forbidden", status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(progressJSON))
}

func progressHarness(t *testing.T) (*cliHarness, *fakeProgressAPI) {
	t.Helper()
	h := newCLIHarness(t)
	api := &fakeProgressAPI{}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	h.url = srv.URL
	h.configure(nil, "a")
	return h, api
}

const progressWebBlock = "web [busy] 마지막 소식 12분 전 — PR 열림\n" +
	"  세션\n" +
	"    web/coord busy …/workspace/web\n" +
	"    web/worker-1 error:api -\n" +
	"  답 없는 지시\n" +
	"  ! #5 → web/coord read 5시간 지남 — 배포 준비\n" +
	"    #6 → web/coord pending 10분 지남 — (제목 없음)\n" +
	"  진행 중 task\n" +
	"    web-12 2일 전 3통 — 시험 추가\n"

func TestProgressCommandText(t *testing.T) {
	h, api := progressHarness(t)
	// Relative times count from generated_at, so the local zone must not matter.
	out := h.mustRun([]string{"TZ=Asia/Seoul"}, "progress")
	want := progressWebBlock + "\n" +
		"api [idle] 보고 없음 (마지막 활동 방금)\n" +
		"  세션\n" +
		"    api idle srv\n" +
		"\n" +
		"old [none] 소식 없음\n"
	// The harness trims surrounding whitespace from stdout.
	if out != strings.TrimSpace(want) {
		t.Fatalf("text output:\n%s\nwant:\n%s", out, want)
	}
	f := func() *http.Request { api.mu.Lock(); defer api.mu.Unlock(); return api.reqs[len(api.reqs)-1] }
	if r := f(); r.Header.Get("Authorization") != "Bearer secret-a" || r.Header.Get("X-Switchboard-Role") != "a" || r.URL.RawQuery != "" {
		t.Fatalf("request: %q %v", r.URL.RawQuery, r.Header)
	}
	if out := h.mustRun(nil, "help"); !strings.Contains(out, "progress") {
		t.Fatalf("help table: %s", out)
	}
}

func TestProgressCommandServiceFilter(t *testing.T) {
	h, _ := progressHarness(t)
	if out := h.mustRun(nil, "progress", "web"); out != strings.TrimSpace(progressWebBlock) {
		t.Fatalf("web only:\n%s", out)
	}
	res := h.run(nil, "progress", "nope")
	if res.code != 1 || !strings.Contains(res.stderr, `"nope"`) || res.stdout != "" {
		t.Fatalf("unknown service: %d stdout=%q stderr=%q", res.code, res.stdout, res.stderr)
	}
	if res := h.run(nil, "progress", "web", "api"); res.code != 2 {
		t.Fatalf("two services: %d %s", res.code, res.stderr)
	}
}

func TestProgressCommandPassthrough(t *testing.T) {
	h, _ := progressHarness(t)
	for _, args := range [][]string{{"progress", "--json"}, {"progress", "web", "--json"}} {
		if out := h.mustRun(nil, args...); out != progressJSON {
			t.Fatalf("%v: %q", args, out)
		}
	}
}

func TestProgressCommandForbidden(t *testing.T) {
	h, api := progressHarness(t)
	api.status = http.StatusForbidden
	for _, args := range [][]string{{"progress"}, {"progress", "--json"}, {"progress", "web"}} {
		res := h.run(nil, args...)
		if res.code == 0 || !strings.Contains(res.stderr, "403") || res.stdout != "" {
			t.Fatalf("%v: %d stdout=%q stderr=%q", args, res.code, res.stdout, res.stderr)
		}
	}
}

func TestProgressDurations(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		ago  time.Duration
		want string
	}{
		{30 * time.Second, "방금"}, {-time.Minute, "방금"}, {90 * time.Second, "2분 전"},
		{59 * time.Minute, "59분 전"}, {90 * time.Minute, "1.5시간 전"}, {11 * time.Hour, "11시간 전"},
		{36 * time.Hour, "1.5일 전"}, {12 * 24 * time.Hour, "12일 전"},
	} {
		if got := relTime(now, now.Add(-c.ago)); got != c.want {
			t.Errorf("relTime(%v) = %q, want %q", c.ago, got, c.want)
		}
	}
	for dir, want := range map[string]string{
		"/home/u/workspace/agent-mail": "…/workspace/agent-mail", "/home/u/workspace/agent-mail/": "…/workspace/agent-mail",
		`C:\Users\u\workspace\web`: "…/workspace/web", "/srv/x": "/srv/x", "": "-",
	} {
		if got := shortDir(dir); got != want {
			t.Errorf("shortDir(%q) = %q, want %q", dir, got, want)
		}
	}
}
