package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/node"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Files `up` keeps in its data directory.
const (
	upDatabase  = "switchboard.db"
	upNodeToken = "node.token"
)

// Up is `switchboard up`: this PC's hosting instance. The server runs with
// hosting on, listening on 127.0.0.1:port, with its database in dataDir; a
// node for this machine runs in the same process and attaches to it over
// loopback HTTP with the node token kept in dataDir (created on first start,
// mode 0600). Every refusal of SWITCHBOARD_HOSTING=on still applies (the
// admin mode must be builtin or oidc). It returns only when start-up fails;
// otherwise it serves until SIGINT or SIGTERM.
func Up(v, dataDir string, port int, nodeName string) error {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("up: data directory: %w", err)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for k, val := range map[string]string{
		config.EnvPrefix + config.HostingName: "on",
		"LISTEN_ADDR":                         addr,
		"DB_PATH":                             filepath.Join(dir, upDatabase),
	} {
		if err = os.Setenv(k, val); err != nil {
			return err
		}
	}
	if _, err = hostingStart(os.Getenv); err != nil {
		return err
	}
	tokenFile := filepath.Join(dir, upNodeToken)
	part, err := prepareNode(v, nodeName, tokenFile, "http://"+addr, dir)
	if err != nil {
		return err
	}
	run(v, func(ctx context.Context, s *Server) error {
		token, err := node.EnsureTokenFile(tokenFile)
		if err != nil {
			return fmt.Errorf("up: node %s: %w", nodeName, err)
		}
		if err = s.hostingService().RegisterLocalNode(nodeName, token); err != nil {
			return fmt.Errorf("up: node %s: %w", nodeName, err)
		}
		link, err := part.link(token)
		if err != nil {
			return err
		}
		log.Printf("node %s: %s backend, socket %s", nodeName, part.backendName, part.socket)
		go func() { _ = link.Run(ctx) }()
		return nil
	})
	return nil
}

// Node is `switchboard node`: a node for this machine attached to the host at
// hostURL with the token in tokenFile, until SIGINT or SIGTERM. With a join
// code it first exchanges the code for a token and writes tokenFile (user
// only; an existing file is never replaced). Only a loopback host is accepted
// in this release.
func Node(v, hostURL, tokenFile, joinCode string) error {
	if err := checkNodeHost(hostURL); err != nil {
		return err
	}
	abs, err := filepath.Abs(tokenFile)
	if err != nil {
		return err
	}
	name := ""
	if joinCode != "" {
		if name, err = joinHost(context.Background(), hostURL, abs, joinCode); err != nil {
			return err
		}
		log.Printf("node %s: joined; token saved to %s", name, abs)
	}
	token, err := node.ReadTokenFile(abs)
	if err != nil {
		return fmt.Errorf("node: token: %w", err)
	}
	part, err := prepareNode(v, name, abs, hostURL, "")
	if err != nil {
		return err
	}
	link, err := part.link(token)
	if err != nil {
		return err
	}
	log.Printf("node: %s backend, socket %s", part.backendName, part.socket)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err = link.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// checkNodeHost accepts an http(s) URL of a loopback host only: nodes on
// other machines wait for the access-scope decision.
func checkNodeHost(hostURL string) error {
	u, err := url.Parse(hostURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("node: --host %q is not an http:// or https:// URL", hostURL)
	}
	if !loopbackHost(u.Hostname()) {
		return fmt.Errorf("node: --host %s is not a loopback address; this release attaches nodes only to a host on this machine (access scope: spec #173 owner answers, decided again before #180)", u.Hostname())
	}
	return nil
}

// joinHost exchanges an invite code at hostURL for the node's token and
// writes it to tokenFile, which must not exist yet. It returns the node name.
func joinHost(ctx context.Context, hostURL, tokenFile, code string) (string, error) {
	if _, err := os.Stat(tokenFile); err == nil {
		return "", fmt.Errorf("node: %s already exists; run without --join to use it, or name another --token-file", tokenFile)
	}
	j, err := node.Join(ctx, hostURL, code)
	if err != nil {
		return "", err
	}
	if err = node.WriteTokenFile(tokenFile, j.Token); err != nil {
		return "", fmt.Errorf("node %s: joined, but the token could not be saved: %w", j.Name, err)
	}
	return j.Name, nil
}

func loopbackHost(h string) bool {
	return loopbackAddr(net.JoinHostPort(h, "0"))
}

// nodePart is a node checked before the host starts: the agent CLI resolves
// and the terminal backend works on this OS.
type nodePart struct {
	cfg         node.Config
	backend     nodeBackend
	backendName string
	socket      string
	hello       wire.NodeHello
	endpoint    string
}

func prepareNode(v, name, tokenFile, endpoint, backendDir string) (*nodePart, error) {
	cfg, err := node.Resolve(node.Machine())
	if err != nil {
		return nil, err
	}
	b, info, err := newNodeBackend(backendOptions{instance: instanceFor(tokenFile), history: cfg.History, dir: backendDir, version: v})
	if err != nil {
		return nil, err
	}
	cfg.Endpoint = endpoint
	cfg.Term = info.term
	if name == "" {
		name = cfg.Machine
	}
	return &nodePart{cfg: cfg, backend: b, backendName: info.name, socket: info.socket, endpoint: endpoint,
		hello: wire.NodeHello{Name: name, Machine: cfg.Machine, OS: runtime.GOOS, Version: v, Backend: info.name}}, nil
}

// backendOptions is what the node's terminal backend is built from.
type backendOptions struct {
	// instance names the backend's server (tmux socket, keeper pipe).
	instance string
	history  int
	// dir holds the backend's files (empty: the user cache directory).
	dir string
	// version is this switchboard's version (the keeper must match it).
	version string
}

// backendInfo describes the backend the node got.
type backendInfo struct {
	name   string // tmux or conpty
	socket string // tmux socket name or keeper pipe
	term   string // TERM given to children ("": the backend's own)
}

// link builds the node over its backend with token as its credential.
func (p *nodePart) link(token string) (*node.Link, error) {
	cfg := p.cfg
	cfg.NodeToken = token
	n, err := node.New(cfg, p.backend)
	if err != nil {
		return nil, err
	}
	return &node.Link{Node: n, Host: p.endpoint, Token: token, Hello: p.hello}, nil
}

// instanceFor names a node's backend server (tmux server, Windows keeper)
// after its token file, so one installation keeps one across restarts and
// two installations on a machine never share one.
func instanceFor(tokenFile string) string {
	sum := sha256.Sum256([]byte(tokenFile))
	return "n" + hex.EncodeToString(sum[:6])
}
