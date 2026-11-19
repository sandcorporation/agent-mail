// Package fakeagent is a stand-in for the agent CLI in tests and demos: a
// test binary re-executed with SWITCHBOARD_FAKE_AGENT=1 behaves like a tiny
// agent. It never calls a real agent.
//
// A test package opts in from TestMain:
//
//	func TestMain(m *testing.M) {
//		fakeagent.MainIfRequested()
//		os.Exit(m.Run())
//	}
//
// Behaviour: it prints "argv <args…>" and one "env KEY" line per variable
// (with "=value" unless the name looks secret), keeps a transcript per
// --session-id under SWITCHBOARD_FAKE_AGENT_DIR so --resume of an unknown id
// fails like the real CLI, then reads input lines: "exit N" exits with N,
// "limit [RESET]" prints the usage-limit message (LimitMessage),
// "draw <scene>" paints SWITCHBOARD_FAKE_AGENT_SCENES/<scene>.vt, anything
// else is echoed as "echo <line>".
package fakeagent

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/node"
	"github.com/sandcorporation/switchboard/internal/term"
)

// Requested reports whether this process should act as the fake agent.
func Requested(getenv func(string) string) bool {
	v, _ := config.EnvFrom(getenv, config.FakeAgentName)
	return v == "1"
}

// MainIfRequested runs the fake agent and exits when the process was
// started as one; otherwise it returns.
func MainIfRequested() {
	if !Requested(os.Getenv) {
		return
	}
	os.Exit(Run(os.Args[1:], os.Environ(), os.Stdin, os.Stdout))
}

// Env returns the variables that turn the current test binary into the
// fake agent keeping transcripts in dir and drawing the term golden scenes.
func Env(dir string) []string {
	return []string{
		config.EnvPrefix + config.FakeAgentName + "=1",
		config.EnvPrefix + config.FakeAgentDirName + "=" + dir,
		config.EnvPrefix + config.FakeAgentScenesName + "=" + ScenesDir(),
	}
}

// ScenesDir is internal/term/testdata/scenes in the source tree this
// package was built from.
func ScenesDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "term", "testdata", "scenes")
}

// Exit codes besides the ones "exit N" asks for.
const (
	ExitUsage         = 2
	ExitUnknownResume = 3
	ExitSessionInUse  = 4
)

// Run is the fake agent: args without the program name, environ as
// "KEY=value", input and output in place of the terminal.
func Run(args, environ []string, in io.Reader, out io.Writer) int {
	getenv := func(k string) string {
		for i := len(environ) - 1; i >= 0; i-- {
			if name, v, ok := strings.Cut(environ[i], "="); ok && name == k {
				return v
			}
		}
		return ""
	}
	fmt.Fprintf(out, "argv %s\r\n", strings.Join(args, " "))
	keys := slices.Clone(environ)
	sort.Strings(keys)
	for _, kv := range keys {
		name, v, _ := strings.Cut(kv, "=")
		if node.SecretName(name) {
			fmt.Fprintf(out, "env %s\r\n", name)
		} else {
			fmt.Fprintf(out, "env %s=%s\r\n", name, v)
		}
	}

	id, resume, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(out, "error: %v\r\n", err)
		return ExitUsage
	}
	dir, _ := config.EnvFrom(getenv, config.FakeAgentDirName)
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "switchboard-fake-agent")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(out, "error: %v\r\n", err)
		return ExitUsage
	}
	path := filepath.Join(dir, id+".transcript")
	flags := os.O_WRONLY | os.O_APPEND
	if resume {
		if _, err := os.Stat(path); err != nil {
			fmt.Fprintf(out, "No conversation found with session ID: %s\r\n", id)
			return ExitUnknownResume
		}
	} else {
		flags |= os.O_CREATE | os.O_EXCL
	}
	tr, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			fmt.Fprintf(out, "Session ID %s is already in use\r\n", id)
			return ExitSessionInUse
		}
		fmt.Fprintf(out, "error: %v\r\n", err)
		return ExitUsage
	}
	defer tr.Close()
	fmt.Fprintf(out, "ready %s\r\n", id)

	scenes, _ := config.EnvFrom(getenv, config.FakeAgentScenesName)
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		fmt.Fprintf(tr, "%s\n", line)
		cmd, arg, _ := strings.Cut(line, " ")
		switch cmd {
		case "exit":
			n, err := strconv.Atoi(strings.TrimSpace(arg))
			if err != nil {
				fmt.Fprintf(out, "error: exit %q\r\n", arg)
				continue
			}
			return n
		case "limit":
			fmt.Fprintf(out, "%s\r\n", LimitMessage(strings.TrimSpace(arg)))
		case "draw":
			if err := draw(out, scenes, strings.TrimSpace(arg)); err != nil {
				fmt.Fprintf(out, "error: %v\r\n", err)
			}
		default:
			fmt.Fprintf(out, "echo %s\r\n", line)
		}
	}
	return 0
}

// parseArgs finds the conversation flag. Exactly one of --session-id and
// --resume is accepted; -p/--print and --continue are refused the way
// hosting never uses them.
func parseArgs(args []string) (id string, resume bool, err error) {
	seen := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-p", "--print", "-c", "--continue":
			return "", false, fmt.Errorf("%s is not allowed", a)
		case "--session-id", "--resume":
			if i+1 >= len(args) || args[i+1] == "" {
				return "", false, fmt.Errorf("%s needs an id", a)
			}
			id, resume = args[i+1], a == "--resume"
			seen++
			i++
		}
	}
	if seen != 1 {
		return "", false, errors.New("want exactly one of --session-id or --resume")
	}
	if strings.ContainsAny(id, `/\.`) {
		return "", false, fmt.Errorf("bad session id %q", id)
	}
	return id, resume, nil
}

func draw(out io.Writer, dir, name string) error {
	if dir == "" || name == "" || strings.ContainsAny(name, `/\.`) {
		return fmt.Errorf("draw: bad scene %q", name)
	}
	raw, err := os.ReadFile(filepath.Join(dir, name+".vt"))
	if err != nil {
		return fmt.Errorf("draw: %w", err)
	}
	sc, err := term.ParseScene(raw)
	if err != nil {
		return fmt.Errorf("draw %s: %w", name, err)
	}
	for _, st := range sc.Steps {
		if st.Data != nil {
			if _, err := out.Write(st.Data); err != nil {
				return err
			}
		}
	}
	return nil
}

// LimitMessage is what "limit [RESET]" prints: the agent's usage-limit
// message, with "resets RESET" when RESET is given (#190).
func LimitMessage(reset string) string {
	if reset == "" {
		return "Claude usage limit reached."
	}
	return "5-hour limit reached ∙ resets " + reset
}
