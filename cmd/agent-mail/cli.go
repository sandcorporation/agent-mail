package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Exit codes shared by every CLI command.
const (
	exitOK      = 0
	exitError   = 1
	exitUsage   = 2
	exitTimeout = 3
)

type Config struct {
	Endpoint string `json:"endpoint"`
	Role     string `json:"role"`
	Token    string `json:"token"`
}

type option struct {
	name string // e.g. "--to"
	arg  string // value placeholder; empty for boolean switches
	help string
}

type command struct {
	name     string
	args     string // positional synopsis, e.g. "ID"
	nargs    int    // exact number of positional arguments
	synopsis string // extra usage text after the positional arguments
	summary  string
	options  []option
}

var (
	optJSON    = option{"--json", "", "print the server's JSON response"}
	optProfile = option{"--profile", "NAME", "use the named credential profile (same as AGENT_MAIL_PROFILE)"}
	optBody    = option{"--body", "TEXT", "message body"}
	optBodyF   = option{"--body-file", "PATH", "read the message body from a file (stdin is used when neither is given)"}
	optSubject = option{"--subject", "TEXT", "message subject"}
)

var commands = []command{
	{name: "register", synopsis: "--endpoint URL --role ROLE --token-file PATH --enrollment-token-file PATH",
		summary: "Register a new role or prove ownership of an existing role",
		options: []option{
			{"--endpoint", "URL", "service URL"},
			{"--role", "ROLE", "role to register"},
			{"--token-file", "PATH", "file holding the role credential"},
			{"--enrollment-token-file", "PATH", "file holding the enrollment credential"},
		}},
	{name: "config", args: "set", nargs: 1, synopsis: "--endpoint URL --role ROLE --token-file PATH",
		summary: "Save endpoint and role credential for this user profile",
		options: []option{
			{"--endpoint", "URL", "service URL"},
			{"--role", "ROLE", "role name"},
			{"--token-file", "PATH", "file holding the role credential"},
			optProfile,
		}},
	{name: "send", synopsis: "--to ROLE --subject TEXT (--body TEXT | --body-file PATH | stdin)",
		summary: "Send a message to a registered role",
		options: []option{
			{"--to", "ROLE", "recipient role"},
			optSubject, optBody, optBodyF,
			{"--task", "ID", "task identifier to attach"},
			{"--idempotency-key", "KEY", "retry-safe key; resending with the same key returns the first message"},
			optJSON, optProfile,
		}},
	{name: "inbox", summary: "List messages for the active role (newest 100)",
		options: []option{
			{"--q", "TEXT", "only messages whose subject, body or task contain TEXT"},
			{"--status", "STATUS", "only messages in STATUS: pending, read, acknowledged or complete (completed also accepted)"},
			optJSON, optProfile,
		}},
	{name: "outbox", summary: "List messages sent by the active role (newest 100)",
		options: []option{
			{"--q", "TEXT", "only messages whose subject, body or task contain TEXT"},
			{"--status", "STATUS", "only messages whose recipient state is STATUS"},
			optJSON, optProfile,
		}},
	{name: "wait", summary: "Wait until the inbox has pending messages, print them and exit (does not mark them read)",
		options: []option{
			{"--timeout", "SECONDS", "give up after SECONDS and exit with code 3 (default 1800, 0 = no limit)"},
			{"--interval", "SECONDS", "seconds between checks (default 15)"},
			optJSON, optProfile,
		}},
	{name: "watch", summary: "Watch inbox without consuming messages",
		options: []option{
			{"--after", "ID", "only report messages with an ID greater than this cursor"},
			optJSON, optProfile,
		}},
	{name: "read", args: "ID", nargs: 1, summary: "Read a message (marks it read)", options: []option{optJSON, optProfile}},
	{name: "reply", args: "ID", nargs: 1, synopsis: "(--body TEXT | --body-file PATH | stdin)", summary: "Reply to a message",
		options: []option{
			optSubject, optBody, optBodyF,
			{"--to", "ROLE", "send the reply to ROLE instead of the original sender"},
			optJSON, optProfile,
		}},
	{name: "ack", args: "ID", nargs: 1, summary: "Acknowledge a message", options: []option{optJSON, optProfile}},
	{name: "complete", args: "ID", nargs: 1, summary: "Mark a message complete", options: []option{optJSON, optProfile}},
	{name: "version", summary: "Show build version"},
}

var validStatuses = map[string]bool{"pending": true, "read": true, "acknowledged": true, "complete": true, "completed": true}

// profileFlag holds the --profile value given on the command line. It wins over AGENT_MAIL_PROFILE.
var profileFlag string

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

func findCommand(name string) (command, bool) {
	for _, c := range commands {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

func main() {
	// Copy the arguments so parsing never rewrites os.Args.
	args := append([]string(nil), os.Args[1:]...)
	if len(args) == 0 {
		runServer()
		return
	}
	os.Exit(runCLI(args))
}

func runCLI(args []string) int {
	code, err := dispatch(args)
	var ue usageError
	if errors.As(err, &ue) {
		fmt.Fprintln(os.Stderr, "agent-mail: "+ue.msg)
		fmt.Fprintln(os.Stderr, "Run 'agent-mail --help' for usage.")
		return exitUsage
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent-mail: "+err.Error())
		return exitError
	}
	return code
}

func dispatch(args []string) (int, error) {
	// Global options before the command.
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		a := args[0]
		switch {
		case a == "-h" || a == "--help":
			printHelp()
			return exitOK, nil
		case a == "--profile":
			if len(args) < 2 {
				return 0, usagef("--profile requires a value")
			}
			profileFlag, args = args[1], args[2:]
		case strings.HasPrefix(a, "--profile="):
			profileFlag, args = strings.TrimPrefix(a, "--profile="), args[1:]
		default:
			return 0, usagef("unknown option %s", a)
		}
	}
	if len(args) == 0 {
		return 0, usagef("missing command")
	}
	name, rest := args[0], args[1:]
	if name == "help" {
		if len(rest) == 0 {
			printHelp()
			return exitOK, nil
		}
		if _, ok := findCommand(rest[0]); !ok {
			return 0, usagef("unknown command %q", rest[0])
		}
		printCommandHelp(rest[0])
		return exitOK, nil
	}
	cmd, ok := findCommand(name)
	if !ok {
		return 0, usagef("unknown command %q", name)
	}
	if hasHelp(rest) || len(rest) > 0 && rest[0] == "help" {
		printCommandHelp(name)
		return exitOK, nil
	}
	p, err := parseCommandArgs(cmd, rest)
	if err != nil {
		return 0, usagef("%s: %s (see 'agent-mail %s --help')", name, err.Error(), name)
	}
	if v, ok := p.values["--profile"]; ok {
		profileFlag = v
	}
	if profileFlag != "" && !validProfile(profileFlag) {
		return 0, usagef("invalid profile name %q", profileFlag)
	}
	if v := os.Getenv("AGENT_MAIL_PROFILE"); profileFlag == "" && v != "" && !validProfile(v) {
		return 0, usagef("invalid profile name %q in AGENT_MAIL_PROFILE", v)
	}
	switch name {
	case "version":
		fmt.Println("agent-mail " + version)
		return exitOK, nil
	case "register":
		return cliRegister(p)
	case "config":
		if p.pos[0] != "set" {
			return 0, usagef("config: unknown subcommand %q (only 'set')", p.pos[0])
		}
		return cliConfig(p)
	}
	if s := p.values["--status"]; s != "" && !validStatuses[s] {
		return 0, usagef("%s: unknown status %q (pending, read, acknowledged, complete)", name, s)
	}
	if cmd.args == "ID" {
		if id, e := strconv.ParseInt(p.pos[0], 10, 64); e != nil || id <= 0 {
			return 0, usagef("%s: invalid message ID %q", name, p.pos[0])
		}
	}
	c, err := loadConfig()
	if err != nil {
		return 0, err
	}
	switch name {
	case "send":
		return cliSend(c, p)
	case "inbox", "outbox":
		return cliList(c, name, p)
	case "wait":
		return cliWait(c, p)
	case "watch":
		return cliWatch(c, p)
	case "read", "ack", "complete":
		method, u := "GET", endpoint(c, "/api/messages/"+p.pos[0])
		if name != "read" {
			method, u = "POST", u+"/"+name
		}
		req, _ := http.NewRequest(method, u, nil)
		return doRequest(c, req, p.bool("--json"))
	case "reply":
		body, e := inputBody(p)
		if e != nil {
			return 0, e
		}
		b, _ := json.Marshal(map[string]any{"subject": p.values["--subject"], "body": body, "recipient": p.values["--to"]})
		req, _ := http.NewRequest("POST", endpoint(c, "/api/messages/"+p.pos[0]+"/reply"), bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		return doRequest(c, req, p.bool("--json"))
	}
	return 0, usagef("unknown command %q", name)
}

type parsedArgs struct {
	values map[string]string
	pos    []string
}

func (p parsedArgs) bool(name string) bool { _, ok := p.values[name]; return ok }

// parseCommandArgs accepts only the options declared for cmd. Unknown options,
// missing values and a wrong number of positional arguments are errors.
func parseCommandArgs(cmd command, args []string) (parsedArgs, error) {
	p := parsedArgs{values: map[string]string{}}
	known := map[string]option{}
	for _, o := range cmd.options {
		known[o.name] = o
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			p.pos = append(p.pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			p.pos = append(p.pos, a)
			continue
		}
		name, value, hasValue := strings.Cut(a, "=")
		o, ok := known[name]
		if !ok {
			return p, fmt.Errorf("unknown option %s", name)
		}
		if o.arg == "" {
			if hasValue {
				return p, fmt.Errorf("option %s takes no value", name)
			}
			p.values[name] = ""
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				return p, fmt.Errorf("option %s requires %s", name, o.arg)
			}
			i++
			value = args[i]
		}
		p.values[name] = value
	}
	if len(p.pos) != cmd.nargs {
		if cmd.nargs == 0 {
			return p, fmt.Errorf("unexpected argument %q", p.pos[0])
		}
		return p, fmt.Errorf("expected %s, got %d argument(s)", cmd.args, len(p.pos))
	}
	return p, nil
}

func hasHelp(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func printHelp() {
	var b strings.Builder
	b.WriteString("agent-mail — role addressed durable mailbox\n\nUsage: agent-mail [--profile NAME] <command> [options]\n\nCommands:\n")
	for _, c := range commands {
		label := c.name
		if c.args != "" {
			label += " " + c.args
		}
		fmt.Fprintf(&b, "  %-12s %s\n", label, c.summary)
	}
	b.WriteString(`  help CMD     Show options for a command

Examples:
  agent-mail register --endpoint https://mail.example --role team/worker --token-file role.token --enrollment-token-file enrollment.token
  agent-mail --profile worker config set --endpoint https://mail.example --role team/worker --token-file role.token
  agent-mail --profile worker send --to team/lead --subject 'Done' --body 'Work is complete'
  agent-mail inbox --status pending --json
  agent-mail wait --timeout 1800

Use 'agent-mail <command> --help' for command options.
--profile NAME may come before or after the command; it overrides AGENT_MAIL_PROFILE.
A named profile that has no saved config is an error.

Exit codes: 0 success, 1 error, 2 usage error (unknown command or option), 3 wait timed out.
`)
	fmt.Print(b.String())
}

func printCommandHelp(name string) {
	c, ok := findCommand(name)
	if !ok {
		printHelp()
		return
	}
	usage := "agent-mail " + c.name
	if c.args != "" {
		usage += " " + c.args
	}
	if c.synopsis != "" {
		usage += " " + c.synopsis
	}
	if len(c.options) > 0 {
		usage += " [options]"
	}
	fmt.Printf("Usage: %s\n\n%s.\n\nOptions:\n", usage, c.summary)
	for _, o := range c.options {
		label := o.name
		if o.arg != "" {
			label += " " + o.arg
		}
		fmt.Printf("  %-28s %s\n", label, o.help)
	}
	fmt.Printf("  %-28s %s\n", "-h, --help", "show this help")
	if c.name == "wait" {
		fmt.Println("\nExit codes: 0 pending messages printed, 3 timed out with none, 1 error.")
	}
}

func configPath() string {
	filename := profileName() + ".json"
	if filename == "default.json" {
		filename = "config.json"
	}
	if d := os.Getenv("AGENT_MAIL_CONFIG_DIR"); d != "" {
		return filepath.Join(d, filename)
	}
	d, e := os.UserConfigDir()
	if e != nil {
		d = "."
	}
	return filepath.Join(d, "agent-mail", filename)
}

// explicitProfile reports whether the user named a profile (flag or environment).
func explicitProfile() bool { return profileName() != "default" }

func profileName() string {
	name := profileFlag
	if name == "" {
		name = os.Getenv("AGENT_MAIL_PROFILE")
	}
	if name == "" {
		return "default"
	}
	if !validProfile(name) {
		fatal("invalid profile name")
	}
	return name
}

func validProfile(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func readConfig() Config {
	b, e := os.ReadFile(configPath())
	var c Config
	if e == nil {
		_ = json.Unmarshal(b, &c)
	}
	if v := os.Getenv("AGENT_MAIL_ENDPOINT"); v != "" {
		c.Endpoint = v
	}
	if v := os.Getenv("AGENT_MAIL_ROLE"); v != "" {
		c.Role = v
	}
	if v := os.Getenv("AGENT_MAIL_TOKEN"); v != "" {
		c.Token = v
	}
	return c
}

// loadConfig returns the active profile's configuration. A profile named on the
// command line or in AGENT_MAIL_PROFILE must exist; it never falls back to another profile.
func loadConfig() (Config, error) {
	p := configPath()
	if explicitProfile() {
		if _, err := os.Stat(p); err != nil {
			return Config{}, fmt.Errorf("profile %q not found (%s); create it with: agent-mail --profile %s config set --endpoint URL --role ROLE --token-file PATH", profileName(), p, profileName())
		}
	}
	c := readConfig()
	if c.Endpoint == "" || c.Token == "" || c.Role == "" {
		return c, fmt.Errorf("profile %q is not configured (%s); run: agent-mail config set --endpoint URL --role ROLE --token-file PATH", profileName(), p)
	}
	return c, nil
}

func cliConfig(p parsedArgs) (int, error) {
	c := readConfig()
	if v := p.values["--endpoint"]; v != "" {
		c.Endpoint = v
	}
	if v := p.values["--role"]; v != "" {
		c.Role = v
	}
	if f := p.values["--token-file"]; f != "" {
		b, e := os.ReadFile(f)
		if e != nil {
			return 0, errors.New("cannot read token file")
		}
		c.Token = strings.TrimSpace(string(b))
	}
	if c.Endpoint == "" || c.Role == "" || c.Token == "" {
		return 0, errors.New("endpoint, role and token required")
	}
	path := configPath()
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	b, _ := json.MarshalIndent(c, "", "  ")
	if os.WriteFile(path, b, 0600) != nil {
		return 0, errors.New("cannot write config")
	}
	fmt.Println("Saved", path)
	return exitOK, nil
}

func cliRegister(p parsedArgs) (int, error) {
	for _, k := range []string{"--endpoint", "--role", "--token-file", "--enrollment-token-file"} {
		if p.values[k] == "" {
			return 0, usagef("register requires --endpoint, --role, --token-file and --enrollment-token-file")
		}
	}
	if !validRole(p.values["--role"]) {
		return 0, errors.New("invalid role name")
	}
	credential, err := os.ReadFile(p.values["--token-file"])
	if err != nil {
		return 0, errors.New("cannot read role credential")
	}
	enrollment, err := os.ReadFile(p.values["--enrollment-token-file"])
	if err != nil {
		return 0, errors.New("cannot read enrollment credential")
	}
	payload, _ := json.Marshal(map[string]string{"role": p.values["--role"], "credential": strings.TrimSpace(string(credential))})
	req, _ := http.NewRequest("POST", strings.TrimRight(p.values["--endpoint"], "/")+"/api/register", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(enrollment)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, errors.New("registration request failed: " + err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("server returned %s: %s", resp.Status, string(b))
	}
	fmt.Print(string(b))
	return exitOK, nil
}

func endpoint(c Config, path string) string { return strings.TrimRight(c.Endpoint, "/") + path }

func cliSend(c Config, p parsedArgs) (int, error) {
	body, e := inputBody(p)
	if e != nil {
		return 0, e
	}
	b, _ := json.Marshal(map[string]any{"recipient": p.values["--to"], "subject": p.values["--subject"], "body": body, "task_id": p.values["--task"]})
	req, _ := http.NewRequest("POST", endpoint(c, "/api/messages"), bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if key := p.values["--idempotency-key"]; key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	return doRequest(c, req, p.bool("--json"))
}

func listURL(c Config, box, q, status string) string {
	v := url.Values{"box": {box}}
	if q != "" {
		v.Set("q", q)
	}
	if status != "" {
		v.Set("status", status)
	}
	return endpoint(c, "/api/messages?"+v.Encode())
}

func cliList(c Config, box string, p parsedArgs) (int, error) {
	req, _ := http.NewRequest("GET", listURL(c, box, p.values["--q"], p.values["--status"]), nil)
	return doRequest(c, req, p.bool("--json"))
}

func inputBody(p parsedArgs) (string, error) {
	if f := p.values["--body-file"]; f != "" {
		b, e := os.ReadFile(f)
		return string(b), e
	}
	if p.values["--body"] != "" {
		return p.values["--body"], nil
	}
	st, e := os.Stdin.Stat()
	if e == nil && st.Mode()&os.ModeCharDevice == 0 {
		b, e := io.ReadAll(os.Stdin)
		return string(b), e
	}
	return "", errors.New("provide --body, --body-file or stdin")
}

func authorize(c Config, req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Agent-Mail-Role", c.Role)
}

func doRequest(c Config, req *http.Request, jsonMode bool) (int, error) {
	authorize(c, req)
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		return 0, e
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("server returned %s: %s", resp.Status, string(b))
	}
	if jsonMode {
		var out bytes.Buffer
		if json.Indent(&out, b, "", "  ") == nil {
			b = out.Bytes()
		}
		fmt.Println(string(b))
		return exitOK, nil
	}
	var v any
	if json.Unmarshal(b, &v) == nil {
		pretty(v)
	} else {
		fmt.Println(string(b))
	}
	return exitOK, nil
}

func pretty(v any) {
	switch x := v.(type) {
	case []any:
		for _, y := range x {
			pretty(y)
			fmt.Println()
		}
	case map[string]any:
		fmt.Printf("#%v %v\n%s → %s · %s\n%s\n", x["id"], x["subject"], x["sender"], x["recipient"], x["status"], x["body"])
	default:
		fmt.Println(v)
	}
}

func secondsOption(p parsedArgs, name string, def int, min int) (time.Duration, error) {
	raw, ok := p.values[name]
	if !ok {
		return time.Duration(def) * time.Second, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < min {
		return 0, usagef("wait: %s must be a whole number of seconds >= %d", name, min)
	}
	return time.Duration(n) * time.Second, nil
}

// errPermanent marks request failures that retrying cannot fix (bad credentials, bad request).
type errPermanent struct{ error }

func fetchPending(client *http.Client, c Config) ([]Message, error) {
	req, _ := http.NewRequest("GET", listURL(c, "inbox", "", "pending"), nil)
	authorize(c, req)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		err := fmt.Errorf("server returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return nil, errPermanent{err}
		}
		return nil, err
	}
	var all []Message
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, fmt.Errorf("unexpected response from server: %v", err)
	}
	pending := []Message{}
	for _, m := range all {
		// Older servers ignore the status filter; filter here as well.
		if m.Status == "pending" {
			pending = append(pending, m)
		}
	}
	return pending, nil
}

// cliWait blocks until the active role has pending inbox messages, prints them and
// exits 0. It never changes message state. With no pending mail before the timeout it exits 3.
func cliWait(c Config, p parsedArgs) (int, error) {
	timeout, err := secondsOption(p, "--timeout", 1800, 0)
	if err != nil {
		return 0, err
	}
	interval, err := secondsOption(p, "--interval", 15, 1)
	if err != nil {
		return 0, err
	}
	jsonMode := p.bool("--json")
	client := &http.Client{Timeout: 30 * time.Second}
	deadline := time.Now().Add(timeout)
	fails := 0
	for {
		msgs, err := fetchPending(client, c)
		var perm errPermanent
		switch {
		case errors.As(err, &perm):
			return 0, perm.error
		case err != nil:
			fails++
			if fails >= 5 {
				return 0, fmt.Errorf("wait: giving up after %d failed checks: %v", fails, err)
			}
		case len(msgs) > 0:
			if jsonMode {
				b, _ := json.MarshalIndent(msgs, "", "  ")
				fmt.Println(string(b))
			} else {
				for _, m := range msgs {
					line := fmt.Sprintf("#%d %s | %s", m.ID, m.Sender, oneLine(m.Subject))
					if m.TaskID != "" {
						line += " | task=" + m.TaskID
					}
					fmt.Println(line)
				}
			}
			return exitOK, nil
		default:
			fails = 0
		}
		sleep := interval
		if timeout > 0 {
			left := time.Until(deadline)
			if left <= 0 {
				if jsonMode {
					fmt.Println("[]")
				}
				fmt.Fprintf(os.Stderr, "agent-mail wait: no pending messages after %s\n", timeout)
				return exitTimeout, nil
			}
			if left < sleep {
				sleep = left
			}
		}
		time.Sleep(sleep)
	}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func cliWatch(c Config, p parsedArgs) (int, error) {
	after, _ := parseID(p.values["--after"])
	jsonMode := p.bool("--json")
	for {
		req, _ := http.NewRequest("GET", fmt.Sprintf("%s?after=%d", endpoint(c, "/api/watch"), after), nil)
		authorize(c, req)
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		var v struct {
			Messages []int64 `json:"messages"`
			Cursor   int64   `json:"cursor"`
		}
		e = json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		if e != nil {
			time.Sleep(time.Second)
			continue
		}
		if v.Cursor > after {
			after = v.Cursor
		}
		for _, id := range v.Messages {
			if jsonMode {
				fmt.Printf("{\"message_id\":%d,\"cursor\":%d}\n", id, after)
			} else {
				fmt.Printf("New message #%d (cursor %d)\n", id, after)
			}
		}
	}
}

func fatal(s string) { fmt.Fprintln(os.Stderr, s); os.Exit(exitError) }
