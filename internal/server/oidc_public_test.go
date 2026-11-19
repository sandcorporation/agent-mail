package server

import (
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/adminauth"
)

// loginOnce runs one code flow against p and returns the callback status.
func loginOnce(t *testing.T, p *fakeIdP) int {
	t.Helper()
	s, now := oidcServer(t, p, "admin@example.com")
	loc, flow := startOIDC(t, s)
	code, state := p.issue(t, loc, *now, nil)
	return callback(s, code, state, flow).Code
}

// #235: without a client secret the instance is a public client. The fake
// provider refuses the token request unless it has no client authentication
// at all (no Authorization header, no client_secret) and a code_verifier
// that matches the challenge.
func TestOIDCPublicClientCodeFlow(t *testing.T) {
	buf := captureLog(t)
	p := newFakeIdP(t)
	p.clientSecret = ""
	if code := loginOnce(t, p); code != 200 {
		t.Fatalf("public client callback=%d", code)
	}
	if p.exchanges != 1 {
		t.Fatalf("exchanges=%d", p.exchanges)
	}
	if !strings.Contains(buf.String(), "oidc: public client (PKCE)") {
		t.Fatalf("mode not logged: %s", buf)
	}

	// The provider really checks PKCE: a code bound to another challenge
	// is refused.
	s, now := oidcServer(t, p, "admin@example.com")
	loc, flow := startOIDC(t, s)
	code, state := p.issue(t, strings.Replace(loc, "code_challenge=", "code_challenge=x", 1), *now, nil)
	if w := callback(s, code, state, flow); w.Code != 403 {
		t.Fatalf("wrong verifier callback=%d", w.Code)
	}
}

// With a secret set nothing changes: the secret goes to the token endpoint,
// and a provider that registered a public client refuses it.
func TestOIDCConfidentialClientSendsSecret(t *testing.T) {
	buf := captureLog(t)
	p := newFakeIdP(t)
	if code := loginOnce(t, p); code != 200 {
		t.Fatalf("confidential client callback=%d", code)
	}
	if !strings.Contains(buf.String(), "oidc: confidential client") {
		t.Fatalf("mode not logged: %s", buf)
	}

	// The same instance against a public-client registration: refused,
	// because it authenticates.
	pub := newFakeIdP(t)
	s := testServer(t)
	env := oidcEnv(pub, "admin@example.com")
	pub.clientSecret = ""
	if err := adminauth.Configure(&s.admin, fakeEnv(env)); err != nil {
		t.Fatal(err)
	}
	loc, flow := startOIDC(t, s)
	code, state := pub.issue(t, loc, time.Now(), nil)
	if w := callback(s, code, state, flow); w.Code != 403 {
		t.Fatalf("secret sent to a public registration: callback=%d", w.Code)
	}
}

// Start-up without a secret succeeds; issuer, client id and admins are still
// required.
func TestOIDCConfigWithoutSecret(t *testing.T) {
	p := newFakeIdP(t)
	p.clientSecret = ""
	full := oidcEnv(p, "admin@example.com")
	if _, ok := full["SWITCHBOARD_OIDC_CLIENT_SECRET"]; ok {
		t.Fatal("public env has a secret")
	}
	if err := adminauth.Configure(&adminauth.Config{}, fakeEnv(full)); err != nil {
		t.Fatalf("public client start: %v", err)
	}
	for _, name := range []string{"SWITCHBOARD_OIDC_ISSUER", "SWITCHBOARD_OIDC_CLIENT_ID", "SWITCHBOARD_OIDC_ADMINS", "SWITCHBOARD_PUBLIC_URL"} {
		env := map[string]string{}
		for k, v := range full {
			if k != name {
				env[k] = v
			}
		}
		if err := adminauth.Configure(&adminauth.Config{}, fakeEnv(env)); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("without %s: %v", name, err)
		}
	}
}

// The ID token's algorithm must be one discovery advertises: EdDSA
// (Ed25519) and RS256 are accepted, anything else is refused.
func TestOIDCIDTokenAlgorithms(t *testing.T) {
	for _, tc := range []struct {
		sign string
		algs []string
		want int
	}{
		{"EdDSA", []string{"EdDSA"}, 200},
		{"RS256", []string{"RS256"}, 200},
		{"EdDSA", []string{"RS256", "EdDSA"}, 200},
		{"RS256", []string{"EdDSA"}, 403},
		{"EdDSA", []string{"RS256"}, 403},
	} {
		p := newFakeIdP(t)
		p.clientSecret = ""
		p.signAlg, p.algs = tc.sign, tc.algs
		if got := loginOnce(t, p); got != tc.want {
			t.Errorf("signed %s, advertised %v: callback=%d want %d", tc.sign, tc.algs, got, tc.want)
		}
	}
}
