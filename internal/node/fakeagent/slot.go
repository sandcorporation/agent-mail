package fakeagent

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// The fake agent's stand-in for the switchboard plugin: with a profile it
// registers its slot at start, heartbeats it, and on "clear" ends it and
// registers the new conversation, the way the plugin does around /clear.

// HeartbeatName is the fake agent's heartbeat interval (Go duration,
// default DefaultHeartbeat).
const HeartbeatName = "FAKE_AGENT_HEARTBEAT"

// DefaultHeartbeat is the fake plugin's heartbeat interval.
const DefaultHeartbeat = 30 * time.Second

// profile is the part of a CLI profile file the fake plugin reads.
type profile struct {
	Endpoint string `json:"endpoint"`
	Role     string `json:"role"`
	Token    string `json:"token"`
}

// plugin holds one slot over HTTP. A nil *plugin does nothing.
type plugin struct {
	endpoint, role, token  string
	machine, label, parent string
	directory              string
	client                 *http.Client

	mu      sync.Mutex
	session string
	granted string // the slot role of the last registration
	muted   bool
	stop    chan struct{}
}

// newPlugin reads SWITCHBOARD_PROFILE from SWITCHBOARD_CONFIG_DIR (the CLI's
// layout: <dir>/<profile>.json) and SWITCHBOARD_ENDPOINT over its endpoint.
// It returns nil when there is no profile to act with.
func newPlugin(getenv func(string) string) (*plugin, error) {
	name, _ := config.EnvFrom(getenv, "PROFILE")
	dir, _ := config.EnvFrom(getenv, "CONFIG_DIR")
	if name == "" || dir == "" || strings.ContainsAny(name, `/\.`) {
		return nil, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", name, err)
	}
	var p profile
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("profile %s: %w", name, err)
	}
	if v, _ := config.EnvFrom(getenv, "ENDPOINT"); v != "" {
		p.Endpoint = v
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	machine, _ := config.EnvFrom(getenv, "MACHINE")
	label, _ := config.EnvFrom(getenv, "LABEL")
	parent, _ := config.EnvFrom(getenv, "PARENT")
	return &plugin{endpoint: strings.TrimRight(p.Endpoint, "/"), role: p.Role, token: p.Token, machine: machine, label: label, parent: parent,
		directory: wd, client: &http.Client{Timeout: 10 * time.Second}, stop: make(chan struct{})}, nil
}

func (p *plugin) call(action, session string) (wire.SlotGrant, error) {
	var g wire.SlotGrant
	body, _ := json.Marshal(wire.SlotRequest{Machine: p.machine, Directory: p.directory, SessionID: session, Label: p.label, Parent: p.parent})
	req, err := http.NewRequest(http.MethodPost, p.endpoint+"/api/slots/"+action, bytes.NewReader(body))
	if err != nil {
		return g, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set(config.HeaderPrefix+"Role", p.role)
	req.Header.Set("Content-Type", "application/json")
	res, err := p.client.Do(req)
	if err != nil {
		return g, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return g, fmt.Errorf("%s: %s", action, res.Status)
	}
	return g, json.NewDecoder(res.Body).Decode(&g)
}

// register registers session and reports the slot role.
func (p *plugin) register(session string) (string, error) {
	p.mu.Lock()
	p.session = session
	p.mu.Unlock()
	g, err := p.call("register", session)
	if err == nil {
		p.mu.Lock()
		p.granted = g.Role
		p.mu.Unlock()
	}
	return g.Role, err
}

// inbox is the slot's inbox (GET /api/messages), newest first, read as its
// slot role the way the plugin checks mail.
func (p *plugin) inbox() ([]wire.Message, error) {
	p.mu.Lock()
	role := p.granted
	p.mu.Unlock()
	if role == "" {
		role = p.role
	}
	req, err := http.NewRequest(http.MethodGet, p.endpoint+"/api/messages?box=inbox", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set(config.HeaderPrefix+"Role", role)
	res, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("inbox: %s", res.Status)
	}
	var out []wire.Message
	return out, json.NewDecoder(res.Body).Decode(&out)
}

// release ends the lease now, as the plugin does when a session ends.
func (p *plugin) release() error {
	p.mu.Lock()
	s := p.session
	p.mu.Unlock()
	_, err := p.call("release", s)
	return err
}

// mute stops the heartbeats, so the lease runs out with the process alive.
func (p *plugin) mute() {
	p.mu.Lock()
	p.muted = true
	p.mu.Unlock()
}

// heartbeat keeps the lease every interval until close.
func (p *plugin) heartbeat(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
		}
		p.mu.Lock()
		s, muted := p.session, p.muted
		p.mu.Unlock()
		if !muted {
			_, _ = p.call("heartbeat", s)
		}
	}
}

func (p *plugin) close() { close(p.stop) }

func heartbeatEvery(getenv func(string) string) time.Duration {
	v, _ := config.EnvFrom(getenv, HeartbeatName)
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	return DefaultHeartbeat
}

// newSessionID is a random (version 4) UUID, the shape of an agent
// conversation id.
func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
