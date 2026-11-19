package adminauth

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/config"
)

// #240: up and serve log the admin access in force once at start-up, in
// every mode, as one "admin auth: …" line. docs/hosting-pc.md and its tests
// look for these lines after a mode change.
func TestConfigureLogsOneAdminAuthLine(t *testing.T) {
	oidcEnv := map[string]string{
		"ADMIN_AUTH":     "oidc",
		"PUBLIC_URL":     "http://127.0.0.1:8090",
		"OIDC_ISSUER":    "https://id.example/realms/home",
		"OIDC_CLIENT_ID": "example-client",
		"OIDC_ADMINS":    "admin@example.com",
	}
	with := func(base map[string]string, k, v string) map[string]string {
		m := map[string]string{k: v}
		for bk, bv := range base {
			if bk != k {
				m[bk] = bv
			}
		}
		return m
	}
	for _, tc := range []struct {
		name  string
		env   map[string]string
		proxy string
		want  string
	}{
		{"builtin", map[string]string{"ADMIN_AUTH": "builtin"}, "", "admin auth: builtin"},
		{"oidc public", oidcEnv, "", "admin auth: oidc (public client, PKCE)"},
		{"oidc confidential", with(oidcEnv, "OIDC_CLIENT_SECRET", "example-secret"), "", "admin auth: oidc (confidential client)"},
		{"off", nil, "", "admin auth: off (" + config.EnvPrefix + "ADMIN_AUTH is not set; admin access is denied)"},
		{"proxy secret", nil, "example-proxy", "admin auth: proxy secret (deprecated: ADMIN_PROXY_TOKEN without " + config.EnvPrefix + "ADMIN_AUTH; set " + config.EnvPrefix + "ADMIN_AUTH to move to admin login)"},
		{"builtin with proxy secret", map[string]string{"ADMIN_AUTH": "builtin"}, "example-proxy", "admin auth: builtin (the proxy secret ADMIN_PROXY_TOKEN is ignored)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev, flags := log.Writer(), log.Flags()
			log.SetOutput(&buf)
			log.SetFlags(0)
			defer func() { log.SetOutput(prev); log.SetFlags(flags) }()
			c := &Config{ProxyToken: tc.proxy}
			if err := Configure(c, func(k string) string { return tc.env[strings.TrimPrefix(k, config.EnvPrefix)] }); err != nil {
				t.Fatal(err)
			}
			if got := buf.String(); got != tc.want+"\n" {
				t.Fatalf("logged %q, want the one line %q", got, tc.want)
			}
			if strings.Contains(buf.String(), "example-secret") || strings.Contains(buf.String(), "example-proxy") {
				t.Fatalf("a secret in the log: %q", buf.String())
			}
		})
	}
}
