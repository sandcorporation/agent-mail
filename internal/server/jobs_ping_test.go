package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/cli"
)

type pingOut struct {
	OK     bool `json:"ok"`
	Exit   int  `json:"exit"`
	Checks []struct {
		Check string `json:"check"`
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	} `json:"checks"`
}

// ping runs job ping --json and checks that its exit code matches the JSON.
func (h *cliHarness) ping(env []string, args ...string) pingOut {
	h.t.Helper()
	r := h.run(env, append(args, "job", "ping", "--json")...)
	var out pingOut
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		h.t.Fatalf("job ping --json: %v\nstdout %q\nstderr %q", err, r.stdout, r.stderr)
	}
	if out.Exit != r.code {
		h.t.Fatalf("exit %d, JSON says %d: %s", r.code, out.Exit, r.stdout)
	}
	return out
}

func failed(o pingOut, check string) bool {
	for _, c := range o.Checks {
		if c.Check == check {
			return !c.OK && c.Error != ""
		}
	}
	return false
}

func TestJobPingWithoutAProfileIsAConfigFailure(t *testing.T) {
	t.Parallel()
	h := newCLIHarness(t)
	o := h.ping(nil, "--profile", "nobody")
	if o.OK || o.Exit != cli.ExitPingConfig || !failed(o, "config") {
		t.Fatalf("ping: %+v", o)
	}
}

// A server that does not answer: ping gives up within its 1 s limit.
func TestJobPingSilentServerIsAServerFailureWithinASecond(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	h := newCLIHarness(t)
	h.mustRun(nil, "config", "set", "--endpoint", srv.URL, "--role", "a", "--token-file", h.tokens["a"])
	start := time.Now()
	o := h.ping(nil)
	if o.OK || o.Exit != cli.ExitPingServer || !failed(o, "server") {
		t.Fatalf("ping: %+v", o)
	}
	// The CLI subprocess starts in well under a second; the request is cut at 0.8 s.
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("ping took %v", d)
	}
}

func TestJobPingRefusedTokenIsATokenFailure(t *testing.T) {
	t.Parallel()
	h := newCLIHarness(t)
	bad := filepath.Join(h.dir, "bad.token")
	if err := os.WriteFile(bad, []byte("not-the-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.mustRun(nil, "config", "set", "--endpoint", h.url, "--role", "a", "--token-file", bad)
	o := h.ping(nil)
	if o.OK || o.Exit != cli.ExitPingToken || !failed(o, "token") || failed(o, "server") {
		t.Fatalf("ping: %+v", o)
	}
	var n int
	if err := h.s.db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("jobs registered: %d %v", n, err)
	}
}

func TestJobPingTextNamesEachCheck(t *testing.T) {
	t.Parallel()
	h := newCLIHarness(t)
	r := h.run(nil, "--profile", "nobody", "job", "ping")
	if r.code != cli.ExitPingConfig || !strings.Contains(r.stderr, "config: ") || !strings.Contains(r.stdout, "data ok") {
		t.Fatalf("code %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
}
