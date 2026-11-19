package server

import (
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/adminauth"
)

// TestOIDCOverLoopbackHTTPWithSecretFile is the oidc sign-in of a hosting PC
// instance end to end: a browser (cookie jar) on plain http to 127.0.0.1,
// the redirect URI http://127.0.0.1:<port>/auth/callback, the client secret
// read from a file, and SWITCHBOARD_INSECURE_COOKIE=1 as the doc's start
// file sets it. Without that setting the browser drops the Secure cookies
// over http and the sign-in fails.
func TestOIDCOverLoopbackHTTPWithSecretFile(t *testing.T) {
	p := newFakeIdP(t)
	secretFile := filepath.Join(t.TempDir(), "oidc-client-secret")
	if err := os.WriteFile(secretFile, []byte(p.clientSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, insecure := range []bool{true, false} {
		s := testServer(t)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		base := "http://" + ln.Addr().String()
		env := oidcEnv(p, "admin@example.com")
		delete(env, "SWITCHBOARD_OIDC_CLIENT_SECRET")
		env["SWITCHBOARD_OIDC_CLIENT_SECRET_FILE"] = secretFile
		env["SWITCHBOARD_PUBLIC_URL"] = base
		if insecure {
			env["SWITCHBOARD_INSECURE_COOKIE"] = "1"
		}
		if err := adminauth.Configure(&s.admin, fakeEnv(env)); err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: s.handler()}
		go func() { _ = srv.Serve(ln) }()

		jar, _ := cookiejar.New(nil)
		var trail []string
		c := &http.Client{Jar: jar, CheckRedirect: func(r *http.Request, via []*http.Request) error {
			trail = append(trail, r.URL.String())
			return nil
		}}
		resp, err := c.Get(base + "/auth/login")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		srv.Close()
		if len(trail) < 2 || !strings.HasPrefix(trail[1], base+"/auth/callback?") || !strings.Contains(trail[0], "redirect_uri="+url.QueryEscape(base+"/auth/callback")) {
			t.Fatalf("insecure=%v redirects %v", insecure, trail)
		}
		var session bool
		for _, ck := range jar.Cookies(mustURL(t, base+"/")) {
			session = session || ck.Name == "sb_session"
		}
		switch {
		case insecure && (resp.StatusCode != 200 || !session):
			t.Fatalf("over loopback http: callback=%d session cookie=%v", resp.StatusCode, session)
		case !insecure && (resp.StatusCode == 200 || session):
			t.Fatalf("without %sINSECURE_COOKIE the sign-in over http worked: %d %v", "SWITCHBOARD_", resp.StatusCode, session)
		}
	}
}
