package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// job ping exit codes, one per cause, in the order the checks are read: the
// first failed check sets the code.
const (
	ExitPingConfig = 10 // the profile is missing or not configured
	ExitPingData   = 11 // the data directory is not writable
	ExitPingServer = 12 // the server did not answer in time, or answered with an error
	ExitPingToken  = 13 // the server refused the role token
	ExitPingRunner = 14 // no runner can start here (Windows: breakaway refused; Linux: a scope with no way out)
)

// pingTimeout bounds the server round trip, so job ping answers within 1 s.
const pingTimeout = 800 * time.Millisecond

type pingCheck struct {
	Check string `json:"check"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	code  int
}

type pingResult struct {
	OK     bool        `json:"ok"`
	Exit   int         `json:"exit"`
	Checks []pingCheck `json:"checks"`
}

// jobPing checks what job run needs, without registering anything: the
// profile, the data directory, the server and the role token, and that a
// runner can leave the caller here. It exits with the first failed check's
// code (0 when all pass).
func jobPing(p parsedArgs) (int, error) {
	if len(p.pos) > 1 || len(p.extra) > 0 {
		return 0, usagef("job ping takes no ID or command")
	}
	var r pingResult
	add := func(name string, code int, err error) {
		c := pingCheck{Check: name, OK: err == nil, code: code}
		if err != nil {
			c.Error = err.Error()
		}
		r.Checks = append(r.Checks, c)
	}
	c, cfgErr := loadConfig()
	add("config", ExitPingConfig, cfgErr)
	add("data", ExitPingData, pingData())
	if cfgErr == nil {
		server, token := pingServer(c)
		add("server", ExitPingServer, server)
		if server == nil {
			add("token", ExitPingToken, token)
		}
	}
	add("runner", ExitPingRunner, checkRunner())

	r.OK = true
	for _, c := range r.Checks {
		if !c.OK && r.OK {
			r.OK, r.Exit = false, c.code
		}
	}
	if p.bool("--json") {
		b, _ := json.Marshal(r)
		fmt.Println(string(b))
		return r.Exit, nil
	}
	for _, c := range r.Checks {
		if c.OK {
			fmt.Println(c.Check + " ok")
		} else {
			fmt.Fprintf(os.Stderr, "%s: %s\n", c.Check, c.Error)
		}
	}
	return r.Exit, nil
}

// pingData writes and removes a file where job output goes.
func pingData() error {
	dir := jobsRoot()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".ping-*")
	if err != nil {
		return err
	}
	name := f.Name()
	err = f.Close()
	if rm := os.Remove(name); err == nil {
		err = rm
	}
	return err
}

var errTokenRefused = errors.New("the server refused the role token")

// pingServer asks the server who this role is. A refusal is a token error;
// no answer in pingTimeout, or any other error status, a server error.
func pingServer(c Config) (server, token error) {
	req, err := http.NewRequest("GET", endpoint(c, "/api/whoami"), nil)
	if err != nil {
		return err, nil
	}
	authorize(c, req)
	resp, err := (&http.Client{Timeout: pingTimeout}).Do(req)
	if err != nil {
		return err, nil
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w (%s)", errTokenRefused, resp.Status)
	case resp.StatusCode >= 300:
		return fmt.Errorf("server returned %s", resp.Status), nil
	}
	return nil, nil
}
