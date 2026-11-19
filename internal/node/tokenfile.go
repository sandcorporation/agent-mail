package node

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// The node's credential (#173) is one token in a file only its user can read:
// mode 0600 on Unix, a protected DACL granting only the current user on
// Windows. The node keeps nothing else durable.

// tokenFileMode keeps a node token readable by its owner only.
const tokenFileMode = 0o600

// joinTimeout bounds the exchange of an invite code for a token.
const joinTimeout = 30 * time.Second

// ReadTokenFile reads a node token file (one line).
func ReadTokenFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("node token file %s is empty", path)
	}
	return token, nil
}

// EnsureTokenFile reads the token in path, creating a new random one there
// when the file is missing. `switchboard up` uses it for its own node.
func EnsureTokenFile(path string) (string, error) {
	token, err := ReadTokenFile(path)
	if !errors.Is(err, os.ErrNotExist) {
		return token, err
	}
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", err
	}
	token = "sbn_" + hex.EncodeToString(b)
	return token, WriteTokenFile(path, token)
}

// WriteTokenFile creates path (never overwriting one) readable by the current
// user only, and writes token to it. The permissions are set before the
// token is written.
func WriteTokenFile(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, tokenFileMode)
	if err != nil {
		return err
	}
	if err = restrictToUser(path); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("node token file %s: %w", path, err)
	}
	if _, err = f.WriteString(token + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// JoinURL is the invite exchange URL for a host base URL.
func JoinURL(host string) (string, error) {
	host = strings.TrimRight(host, "/")
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		return "", errors.New("node: host URL must start with http:// or https://")
	}
	return host + "/api/node/join", nil
}

// Join exchanges a one-time invite code (`switchboard node invite`) for the
// node's name and token. The host answers each code once, within 15 minutes.
func Join(ctx context.Context, host, code string) (wire.NodeJoined, error) {
	var out wire.NodeJoined
	url, err := JoinURL(host)
	if err != nil {
		return out, err
	}
	body, _ := json.Marshal(wire.NodeJoin{Code: strings.TrimSpace(code)})
	ctx, cancel := context.WithTimeout(ctx, joinTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, fmt.Errorf("node: join: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("node: join refused (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if err = json.Unmarshal(b, &out); err != nil || out.Name == "" || out.Token == "" {
		return wire.NodeJoined{}, errors.New("node: join: the host's answer has no node name and token")
	}
	return out, nil
}
