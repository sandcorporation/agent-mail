package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/cli"
)

const decisionsJSON = `{"decisions":[` +
	`{"id":"q-2","asker":"team/worker-2","asked_at":"2026-11-19T09:00:00Z","answered_at":"2026-11-19T10:30:00Z","answered_by":"sandcorp/boss","items":[{"header":"배포","question":"배포할까요?","chosen":["예"],"other":""}]},` +
	`{"id":"q-1","asker":"a","asked_at":"2026-11-17T09:00:00Z","answered_at":"2026-11-17T10:00:00Z","answered_by":"sandcorp/boss","items":[{"header":"진행","question":"진행할까요?","chosen":["예"],"other":""},{"header":"범위","question":"범위는?","chosen":["web","api"],"other":"천천히"},{"header":"메모","question":"메모?","chosen":[],"other":"나중에"}]}` +
	`]}`

const decisionsNDJSON = `{"id":"q-2","asker":"team/worker-2"}` + "\n" + `{"id":"q-1","asker":"a"}` + "\n"

// fakeDecisionsAPI stands in for GET /api/decisions: it records each request
// and answers status with the canned JSON, or the ndjson export when
// format=ndjson.
type fakeDecisionsAPI struct {
	mu     sync.Mutex
	status int
	reqs   []*http.Request
}

func (f *fakeDecisionsAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	status := f.status
	f.mu.Unlock()
	if r.URL.Path != "/api/decisions" || r.Method != "GET" {
		http.NotFound(w, r)
		return
	}
	if status != 0 {
		http.Error(w, "forbidden", status)
		return
	}
	if r.URL.Query().Get("format") == "ndjson" {
		w.Header().Set("Content-Type", ndjsonMimeType)
		w.Write([]byte(decisionsNDJSON))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(decisionsJSON))
}

func (f *fakeDecisionsAPI) last(t *testing.T) *http.Request {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("no request reached the server")
	}
	return f.reqs[len(f.reqs)-1]
}

func decisionsHarness(t *testing.T) (*cliHarness, *fakeDecisionsAPI) {
	t.Helper()
	h := newCLIHarness(t)
	api := &fakeDecisionsAPI{}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	h.url = srv.URL
	h.configure(nil, "a")
	return h, api
}

func TestDecisionsCommandParameters(t *testing.T) {
	t.Parallel()
	h, api := decisionsHarness(t)
	h.mustRun(nil, "decisions")
	r := api.last(t)
	if r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer secret-a" || r.Header.Get("X-Switchboard-Role") != "a" {
		t.Fatalf("bare request: %q %v", r.URL.RawQuery, r.Header)
	}
	h.mustRun(nil, "decisions", "--from", "2026-11-01", "--to", "2026-11-17T12:00:00+09:00", "--role", "team/*", "--q", "배포 계획")
	got := api.last(t).URL.Query()
	want := url.Values{"from": {"2026-11-01"}, "to": {"2026-11-17T12:00:00+09:00"}, "role": {"team/*"}, "q": {"배포 계획"}}
	if got.Encode() != want.Encode() {
		t.Fatalf("query: %v want %v", got, want)
	}
	if res := h.run(nil, "decisions", "--json", "--ndjson"); res.code != cli.ExitUsage {
		t.Fatalf("--json --ndjson: %d %s", res.code, res.stderr)
	}
	if out := h.mustRun(nil, "help", "decisions"); !strings.Contains(out, "--role ROLE") || !strings.Contains(out, "--ndjson") {
		t.Fatalf("help: %s", out)
	}
	if out := h.mustRun(nil, "help"); !strings.Contains(out, "decisions") {
		t.Fatalf("help table: %s", out)
	}
}

func TestDecisionsCommandText(t *testing.T) {
	t.Parallel()
	h, _ := decisionsHarness(t)
	out := h.mustRun([]string{"TZ=Asia/Seoul"}, "decisions")
	// The CLI prints local time; Go reads TZ only on Unix, so on Windows
	// the child keeps the machine's zone, which is this process's too.
	q2, q1 := "2026-11-19 19:30", "2026-11-17 19:00"
	if runtime.GOOS == "windows" {
		q2 = time.Date(2026, 11, 19, 10, 30, 0, 0, time.UTC).Local().Format("2006-01-02 15:04")
		q1 = time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC).Local().Format("2006-01-02 15:04")
	}
	want := "q-2 " + q2 + " team/worker-2 (by sandcorp/boss)\n" +
		"  배포: 예\n" +
		"\n" +
		"q-1 " + q1 + " a (by sandcorp/boss)\n" +
		"  진행: 예\n" +
		"  범위: web, api / 기타: 천천히\n" +
		"  메모: 기타: 나중에\n"
	// The harness trims surrounding whitespace from stdout.
	if out != strings.TrimSpace(want) {
		t.Fatalf("text output:\n%s\nwant:\n%s", out, want)
	}
}

func TestDecisionsCommandPassthrough(t *testing.T) {
	t.Parallel()
	h, api := decisionsHarness(t)
	if out := h.mustRun(nil, "decisions", "--json"); out != decisionsJSON {
		t.Fatalf("--json: %q", out)
	}
	out := h.mustRun(nil, "decisions", "--ndjson", "--role", "a")
	if out != strings.TrimSpace(decisionsNDJSON) {
		t.Fatalf("--ndjson: %q", out)
	}
	if q := api.last(t).URL.Query(); q.Get("format") != "ndjson" || q.Get("role") != "a" {
		t.Fatalf("ndjson query: %v", q)
	}
}

func TestDecisionsCommandForbidden(t *testing.T) {
	t.Parallel()
	h, api := decisionsHarness(t)
	api.mu.Lock()
	api.status = http.StatusForbidden
	api.mu.Unlock()
	for _, args := range [][]string{{"decisions"}, {"decisions", "--json"}, {"decisions", "--ndjson"}} {
		res := h.run(nil, args...)
		if res.code == 0 || !strings.Contains(res.stderr, "403") || res.stdout != "" {
			t.Fatalf("%v: %d stdout=%q stderr=%q", args, res.code, res.stdout, res.stderr)
		}
	}
}
