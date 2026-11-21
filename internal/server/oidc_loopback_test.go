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
// file sets it. What the server decides is the Secure attribute of the
// session cookie: set without the setting, absent with it. Whether a client
// then sends a Secure cookie to http://127.0.0.1 is the client's rule (Go's
// cookiejar changed it in 1.26, #251), so the test asserts the header and
// only the sign-in with the setting end to end.
func TestOIDCOverLoopbackHTTPWithSecretFile(t *testing.T) {
	t.Parallel()
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
		var setCookies []string // Set-Cookie headers the server wrote
		h := s.handler()
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(&cookieSpy{ResponseWriter: w, got: &setCookies}, r)
		})}
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
		// The login always sets sb_oidc; the session cookie only when the
		// client sent sb_oidc back, which a jar may refuse over http.
		var oidcSet bool
		for _, line := range setCookies {
			name, _, _ := strings.Cut(line, "=")
			if (name == "sb_oidc" || name == "sb_session") && !strings.Contains(line, "Max-Age=0") {
				oidcSet = true
				if secure := strings.Contains(line, "; Secure"); secure == insecure {
					t.Fatalf("insecure=%v: %s Secure=%v in %q", insecure, name, secure, line)
				}
			}
		}
		if !oidcSet {
			t.Fatalf("insecure=%v: no cookie set: %q", insecure, setCookies)
		}
		if insecure && (resp.StatusCode != 200 || !session) {
			t.Fatalf("over loopback http: callback=%d session cookie=%v", resp.StatusCode, session)
		}
	}
}

// cookieSpy records the Set-Cookie headers of one response when it starts.
type cookieSpy struct {
	http.ResponseWriter
	got  *[]string
	done bool
}

func (c *cookieSpy) record() {
	if !c.done {
		c.done = true
		*c.got = append(*c.got, c.Header().Values("Set-Cookie")...)
	}
}

func (c *cookieSpy) WriteHeader(code int) {
	c.record()
	c.ResponseWriter.WriteHeader(code)
}

func (c *cookieSpy) Write(b []byte) (int, error) {
	c.record()
	return c.ResponseWriter.Write(b)
}
