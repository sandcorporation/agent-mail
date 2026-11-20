package cli

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/wire"
)

func healthServer(t *testing.T, body string, seen *string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		if seen != nil {
			*seen = r.Header.Get("X-Switchboard-Version")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestVersionCheckExitCodes(t *testing.T) {
	asset := wire.ReleaseAsset("v0.26.0", runtime.GOOS, runtime.GOARCH)
	for _, c := range []struct {
		name, health, this string
		code               int
		out                string
	}{
		{"current", `{"ok":true,"version":"v0.26.0","cli_min":"v0.26"}`, "v0.26.0", ExitOK, "server v0.26.0, this v0.26.0\n"},
		{"newer", `{"ok":true,"version":"v0.26.0","cli_min":"v0.26"}`, "v0.27.1", ExitOK, "server v0.26.0, this v0.27.1\n"},
		{"behind", `{"ok":true,"version":"v0.26.0","cli_min":"v0.26"}`, "v0.16.0", ExitBehind, "server v0.26.0, this v0.16.0\nupdate: " + asset + "\n"},
		{"lower minimum", `{"ok":true,"version":"v0.26.0","cli_min":"v0.20"}`, "v0.21.0", ExitOK, "server v0.26.0, this v0.21.0\n"},
		{"server before #308", `{"ok":true,"version":"v0.26.0"}`, "v0.25.0", ExitBehind, "server v0.26.0, this v0.25.0\nupdate: " + asset + "\n"},
		{"dev build", `{"ok":true,"version":"v0.26.0","cli_min":"v0.26"}`, "dev", ExitOK, "server v0.26.0, this dev\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			old := version
			version = c.this
			defer func() { version = old }()
			var seen string
			url := healthServer(t, c.health, &seen)
			var out strings.Builder
			code, err := versionCheck(Config{Endpoint: url + "/"}, c.this, &out)
			if err != nil || code != c.code || out.String() != c.out {
				t.Fatalf("code %d err %v out %q; want %d %q", code, err, out.String(), c.code, c.out)
			}
			if seen != c.this {
				t.Fatalf("version header %q, want %q", seen, c.this)
			}
		})
	}
}

func TestVersionCheckUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	var out strings.Builder
	code, err := versionCheck(Config{Endpoint: url}, "v0.26.0", &out)
	if err == nil || code == ExitBehind || out.Len() != 0 {
		t.Fatalf("code %d err %v out %q", code, err, out.String())
	}
	if !strings.Contains(err.Error(), "cannot reach the server") || strings.Contains(err.Error(), "\n") {
		t.Fatalf("error %q", err)
	}
}

func TestEveryRequestSendsVersion(t *testing.T) {
	old := version
	version = "v0.26.0"
	defer func() { version = old }()
	req := httptest.NewRequest("GET", "/api/inbox", nil)
	authorize(Config{Token: "t", Role: "team/boss"}, req)
	if got := req.Header.Get("X-Switchboard-Version"); got != "v0.26.0" {
		t.Fatalf("version header %q", got)
	}
}
