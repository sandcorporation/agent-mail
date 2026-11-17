package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// cliInvite is `switchboard invite --prefix P [--role R] [--ttl 72h]`: it
// prints the code and a join line to paste to the collaborator.
func cliInvite(c Config, p parsedArgs, stdout io.Writer) (int, error) {
	prefix := p.values["--prefix"]
	if prefix == "" {
		return 0, usagef("invite requires --prefix")
	}
	payload, _ := json.Marshal(inviteRequest{Prefix: prefix, Role: p.values["--role"], TTL: p.values["--ttl"]})
	req, _ := http.NewRequest("POST", endpoint(c, "/api/invites"), bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	authorize(c, req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("server returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if p.bool("--json") {
		var out bytes.Buffer
		if json.Indent(&out, b, "", "  ") == nil {
			b = out.Bytes()
		}
		fmt.Fprintln(stdout, string(b))
		return exitOK, nil
	}
	var v inviteCreated
	if err = json.Unmarshal(b, &v); err != nil || v.Code == "" {
		return 0, errors.New("unexpected server response")
	}
	line := progName() + " join " + strings.TrimRight(c.Endpoint, "/") + " " + v.Code
	if v.Role != nil {
		line += " --role " + *v.Role
	} else {
		line += " --role " + v.Prefix + "/NAME"
	}
	fmt.Fprintf(stdout, "Code:    %s\nGroup:   %s\nExpires: %s\nJoin:    %s\n", v.Code, v.Prefix, v.ExpiresAt, line)
	return exitOK, nil
}

// cliJoin is `switchboard join URL CODE [--role R] [--profile NAME]`. It
// makes the role token here (crypto/rand), registers it with the code and
// writes it to a new profile file (mode 0600). The token is never printed.
func cliJoin(p parsedArgs, stdout io.Writer) (int, error) {
	base, code := strings.TrimRight(p.pos[0], "/"), strings.TrimSpace(p.pos[1])
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return 0, usagef("join: URL must be an http(s) server URL")
	}
	if code == "" {
		return 0, usagef("join: CODE is empty")
	}
	if r := p.values["--role"]; r != "" && !validRole(r) {
		return 0, errors.New("invalid role name")
	}
	name := profileFlag
	if name == "" {
		name = envAlias("PROFILE")
	}
	if name == "" {
		name = hostProfile(u.Hostname())
	}
	if !validProfile(name) {
		return 0, usagef("invalid profile name %q", name)
	}
	profileFlag = name
	path := configPath()
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return 0, errors.New("cannot create config directory")
	}
	// Claim the file first so a taken name or an unwritable directory fails
	// before the code is spent.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return 0, fmt.Errorf("profile %q already exists (%s); choose another with --profile NAME", name, path)
	}
	if err != nil {
		return 0, errors.New("cannot write profile " + path)
	}
	written := false
	defer func() {
		f.Close()
		if !written {
			os.Remove(path)
		}
	}()
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return 0, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	payload, _ := json.Marshal(joinRequest{Code: code, Role: p.values["--role"], Credential: token})
	req, _ := http.NewRequest("POST", base+"/api/join", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, errors.New("join request failed: " + err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("server returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		Role     string `json:"role"`
		Group    string `json:"group"`
		BossRole string `json:"boss_role"`
	}
	if err = json.Unmarshal(b, &out); err != nil || out.Role == "" {
		return 0, errors.New("unexpected server response")
	}
	cfg, _ := json.MarshalIndent(Config{Endpoint: base, Role: out.Role, Token: token}, "", "  ")
	if _, err = f.Write(cfg); err != nil {
		return 0, fmt.Errorf("joined as %s but cannot write profile %s", out.Role, path)
	}
	if err = f.Close(); err != nil {
		return 0, fmt.Errorf("joined as %s but cannot write profile %s", out.Role, path)
	}
	written = true
	envLine := envPrefix + "PROFILE=" + name
	if d := envAlias("CONFIG_DIR"); d != "" {
		envLine = envPrefix + "CONFIG_DIR=" + d + " " + envLine
	}
	fmt.Fprintf(stdout, "Role:    %s\nGroup:   %s (boss %s)\nProfile: %s\n%s\n", out.Role, out.Group, out.BossRole, path, envLine)
	return exitOK, nil
}

// hostProfile turns a server host into a profile name: mail.example becomes
// mail-example.
func hostProfile(host string) string {
	var b strings.Builder
	for _, c := range host {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 64 {
		name = name[:64]
	}
	if name == "" {
		name = "switchboard"
	}
	return name
}
