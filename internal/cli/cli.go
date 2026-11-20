// Package cli is the client: every switchboard command, the
// credential profiles and the HTTP calls. It talks to the server over HTTP only.
package cli

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

	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/ownerfile"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// Exit codes shared by every CLI command.
const (
	ExitOK      = 0
	ExitError   = 1
	ExitUsage   = 2
	ExitTimeout = 3
	// ExitNodeRevoked: `node` stopped because the host refused its token
	// (revoked) (#203).
	ExitNodeRevoked = 4
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
	nargs    int    // exact number of positional arguments (at most, with optArgs)
	optArgs  bool   // the positional arguments may be left out
	extra    bool   // arguments after -- are passed through (parsedArgs.extra), not positional
	synopsis string // extra usage text after the positional arguments
	summary  string
	options  []option
}

var (
	optJSON    = option{"--json", "", "print the server's JSON response"}
	optProfile = option{"--profile", "NAME", "use the named credential profile (same as SWITCHBOARD_PROFILE)"}
	optBody    = option{"--body", "TEXT", "message body"}
	optBodyF   = option{"--body-file", "PATH", "read the message body from a file (stdin is used when neither is given)"}
	optSubject = option{"--subject", "TEXT", "message subject"}
	optUrgent  = option{"--urgent", "", "mark the message urgent (default priority is normal)"}
	optNoReply = option{"--no-reply", "", "the message needs no answer: never an open directive, never reminded"}
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
	{name: "join", args: "URL CODE", nargs: 2,
		summary: "Join a server with an invite code: makes the role token here and saves a profile",
		options: []option{
			{"--role", "ROLE", "role to take (a name under the invite's prefix; the invite's role when it names one)"},
			{"--profile", "NAME", "profile to write (default: from the server host); must not exist yet"},
		}},
	{name: "invite", synopsis: "--prefix PREFIX [--role ROLE] [--ttl 72h]",
		summary: "Create a one-time invite code for a collaborator group (global boss roles only)",
		options: []option{
			{"--prefix", "PREFIX", "group prefix, e.g. partner-a; the group is created when missing"},
			{"--role", "ROLE", "pin the invite to one role under the prefix"},
			{"--ttl", "DURATION", "how long the code works, e.g. 72h or 7d (default 72h, at most 30d)"},
			optJSON, optProfile,
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
			{"--to", "ROLE", "recipient role, or label:BASE/LABEL for the session of BASE carrying LABEL"},
			optSubject, optBody, optBodyF,
			{"--task", "ID", "task identifier to attach"},
			optUrgent, optNoReply,
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
	{name: "events", summary: "Long-poll once for new pending mail, print it and exit (does not mark it read)",
		options: []option{
			{"--after", "ID", "only messages with an ID greater than this cursor (default: every pending message)"},
			{"--timeout", "SECONDS", "wait up to SECONDS for new mail, capped by the server (default 25, 0 = answer at once); exit 3 when none arrives"},
			{"--json", "", "print the server's JSON response ({\"events\":[...],\"cursor\":N})"},
			optProfile,
		}},
	{name: "watch", args: "[start]", nargs: 1, optArgs: true, extra: true, synopsis: "[-- CMD ARGS...]",
		summary: "Watch inbox without consuming messages; watch start runs a command on this machine whose output lines are events (lines within a batch window make one urgent mail to this role; list, stop and wait it with job)",
		options: []option{
			{"--after", "ID", "only report messages with an ID greater than this cursor"},
			{"--attach", "", "start: stream the lines to stdout instead of mail while attached (killing it leaves the watch running and mail takes over)"},
			{"--expires", "DURATION", "start: how long it may run, e.g. 90m, 24h, 7d (default 7d; moves forward while a session of this role is attached)"},
			{"--batch", "DURATION", "start: batch window, lines within it make one mail (default 30s)"},
			{"--cwd", "DIR", "start: working directory (default the current one)"},
			{"--label", "TEXT", "start: label (default the first 60 characters of the command)"},
			{"--no-extend", "", "start: the expiry is fixed; it does not move forward while a session is attached"},
			{"--idem", "KEY", "start: retry-safe key; starting again with the same key returns the first watch"},
			optJSON, optProfile,
		}},
	{name: "read", args: "ID", nargs: 1, summary: "Read a message (marks it read)", options: []option{optJSON, optProfile}},
	{name: "reply", args: "ID", nargs: 1, synopsis: "[--done] (--body TEXT | --body-file PATH | stdin)", summary: "Reply to a message; reply --done with the final report closes the directive",
		options: []option{
			{"--done", "", "the reply is the final report: also mark the directive done (one transaction). Without it the directive stays open"},
			optSubject, optBody, optBodyF,
			{"--to", "ROLE", "send the reply to ROLE instead of the original sender"},
			optUrgent,
			{"--directive", "", "the reply carries new work: count it as a directive in progress"},
			optNoReply,
			{"--idempotency-key", "KEY", "retry-safe key; resending with the same key returns the first reply and marks nothing twice"},
			optJSON, optProfile,
		}},
	{name: "ack", args: "ID", nargs: 1, summary: "Acknowledge a message", options: []option{optJSON, optProfile}},
	{name: "complete", args: "ID", nargs: 1, summary: "Mark a message complete", options: []option{optJSON, optProfile}},
	{name: "close", args: "ID", nargs: 1, summary: "Close a directive you sent: it needs no answer (no longer open, never reminded)", options: []option{optJSON, optProfile}},
	{name: "slots", summary: "List the leased numbered slots under the active role's base",
		options: []option{optJSON, optProfile}},
	{name: "slot", args: "ACTION", nargs: 1, synopsis: "(ACTION: register | heartbeat | takeover | release)",
		summary: "Claim, renew, take over (-1) or release a numbered slot for this machine and directory",
		options: []option{
			{"--machine", "NAME", "machine identity (default: this host name)"},
			{"--directory", "PATH", "directory identity (default: the current directory)"},
			{"--session-id", "ID", "session id to record with the slot"},
			{"--label", "TEXT", "label to record with the slot"},
			{"--parent", "ROLE", "parent role to record with the slot"},
			{"--state", "STATE", "presence state to report: idle, busy, compacting, error:<kind> or blocked:<kind> (kind: 1-64 of A-Z a-z 0-9 . _ -) (register, heartbeat, takeover)"},
			{"--context-tokens", "N", "context size in tokens to report (register, heartbeat, takeover)"},
			{"--last-turn-at", "TIME", "time the last turn ended, RFC 3339 (register, heartbeat, takeover)"},
			{"--subagents", "N", "running background sub-agents to report; leaving it out reports 0 (register, heartbeat, takeover)"},
			optJSON, optProfile,
		}},
	{name: "status", args: "[ROLE]", nargs: 1, optArgs: true,
		summary: "Show session presence: every session, a base and its numbers, one numbered role, or a prefix ending in *",
		options: []option{optJSON, optProfile}},
	{name: "metrics",
		summary: "Show mail-flow metrics: first-reply time, pending mail, back-and-forth threads, error/blocked session time (boss roles only)",
		options: []option{
			{"--days", "N", "window in days ending now, 1-90 (default 7)"},
			optJSON, optProfile,
		}},
	{name: "progress", args: "[SERVICE]", nargs: 1, optArgs: true,
		summary: "Show per-service progress: last report, live sessions, and open directives (! over the threshold) (boss roles only)",
		options: []option{{"--json", "", "print the server's whole JSON response unchanged (SERVICE is ignored)"}, optProfile}},
	{name: "report", synopsis: "--title TEXT [--request TEXT] [--mail ID[,ID...]]... (--body TEXT | --body-file PATH | stdin)",
		summary: "Post a report to the dashboard, linking the directive threads it covers (boss roles only); prints the report ID",
		options: []option{
			{"--title", "TEXT", "report title (one line)"},
			{"--request", "TEXT", "what was asked (one line)"},
			{"--mail", "ID[,ID...]", "link mail by ID (repeatable); a reply links its thread's directive; #N in the body is never a link"},
			{"--body", "TEXT", "report body (Markdown)"},
			{"--body-file", "PATH", "read the report body from a file (stdin is used when neither is given)"},
			{"--idem", "KEY", "retry-safe key; posting again with the same key returns the first report"},
			{"--json", "", "print the stored report as JSON"},
			optProfile,
		}},
	{name: "decisions",
		summary: "Show the decision ledger (answered questions), newest first; a boss role sees all, other roles their own base",
		options: []option{
			{"--from", "DATE|TIME", "answered on or after this UTC date (YYYY-MM-DD) or RFC 3339 time"},
			{"--to", "DATE|TIME", "answered up to this UTC date (inclusive) or RFC 3339 time"},
			{"--role", "ROLE", "asker: an exact role or base, or PREFIX/*"},
			{"--q", "TEXT", "case-insensitive text in a header, question or chosen label"},
			{"--ndjson", "", "stream the server's ndjson export unchanged"},
			optJSON, optProfile,
		}},
	{name: "permission", args: "ACTION [ID]", nargs: 2, optArgs: true, synopsis: "(ACTION: request | show ID | answer ID | list)",
		summary: "Ask for, show, answer or list tool permission requests (only a boss role answers; the requester's parent is notified)",
		options: []option{
			{"--tool", "NAME", "request: the tool asked for"},
			{"--summary", "TEXT", "request: what the tool will do (up to 500 bytes)"},
			{"--decision", "allow|deny", "answer: the decision"},
			{"--note", "TEXT", "answer: optional note for the requester"},
			{"--pending", "", "list: only requests still waiting for an answer"},
			optJSON, optProfile,
		}},
	{name: "question", args: "ACTION [ID]", nargs: 2, optArgs: true, synopsis: "(ACTION: ask | show ID | list | answer ID | withdraw ID)",
		summary: "Ask, show, list, answer or withdraw multiple-choice questions (only a boss role of another base answers; first answer wins)",
		options: []option{
			{"--file", "PATH", "ask: JSON with the questions ({\"questions\":[...]} or the bare list); answer: JSON with the answers ({\"answers\":[...]} or the bare list)"},
			{"--status", "STATUS", "list: only pending, answered or withdrawn questions"},
			optJSON, optProfile,
		}},
	{name: "host", args: "ACTION [ID]", nargs: 2, optArgs: true, extra: true, synopsis: "(ACTION: add | list | set ID | start ID | stop ID | restart ID | new-conversation ID | remove ID | limit [on|off]) [-- AGENT ARGS...]",
		summary: "Manage hosted sessions in this instance's database (DB_PATH, not the container default): add a record, list them, change one's permission mode, set one to run or stop, restart one with resume (clears a crash loop), start one in a new conversation, remove one, show or set the subscription limit",
		options: []option{
			{"--node", "NAME", "add: node the session runs on"},
			{"--base", "ROLE", "add: base role (must exist)"},
			{"--profile", "NAME", "add: credential profile on the node (SWITCHBOARD_PROFILE)"},
			{"--dir", "PATH", "add: absolute working directory on the node"},
			{"--label", "TEXT", "add: label (SWITCHBOARD_LABEL)"},
			{"--parent", "ROLE", "add: parent role (SWITCHBOARD_PARENT)"},
			{"--restart", "POLICY", "add: always (default), on-failure or never"},
			{"--class", "CLASS", "add: lead, coordinator, execute (default) or review"},
			{"--ask-permissions", "", "add: launch the agent in its normal permission mode (default: with its permission-skip arguments)"},
			{"--skip-permissions", "on|off", "set: launch with (on) or without (off) the agent's permission-skip arguments, from the next launch or restart"},
			optJSON,
		}},
	{name: "up",
		summary: "Run this PC's hosting instance: the server with terminal hosting on, on 127.0.0.1:PORT with its database in DIR, and a node for this machine in the same process",
		options: []option{
			{"--data", "DIR", "data directory: database, node token, tmux config (created if missing)"},
			{"--port", "PORT", "loopback port to listen on"},
			{"--node", "NAME", "name of this machine's node, as host add --node names it (default local)"},
		}},
	{name: "node", args: "[invite NAME]", nargs: 2, optArgs: true,
		summary: "Run a node for this machine attached to a hosting instance (loopback only in this release); node invite NAME prints a one-time join code (15 min) from this instance's database (DB_PATH)",
		options: []option{
			{"--host", "URL", "the instance's URL, e.g. http://127.0.0.1:8090"},
			{"--join", "CODE", "exchange a code from node invite for this node's token, saved to --token-file"},
			{"--token-file", "PATH", "node token file (default node.token in the config directory; up writes DIR/node.token)"},
			{"--restart-sessions", "", "stop every hosted session on this machine at start and relaunch each with resume"},
		}},
	{name: "admin", args: "ACTION NAME", nargs: 2, synopsis: "(ACTION: create | passwd | totp-reset | disable) NAME",
		summary: "Manage built-in dashboard admins in the server database (DB_PATH; run inside the server container); create and passwd read the password from the terminal or the first line of stdin",
		options: []option{optJSON}},
	{name: "version", summary: "Show build version"},
}

var validStatuses = map[string]bool{"pending": true, "read": true, "acknowledged": true, "complete": true, "completed": true}

// profileFlag holds the --profile value given on the command line. It wins over SWITCHBOARD_PROFILE.
var profileFlag string

// ExitCodeError is an error from a command the binary supplies that exits
// with Code instead of ExitError.
type ExitCodeError struct {
	Code int
	Err  error
}

func (e ExitCodeError) Error() string { return e.Err.Error() }
func (e ExitCodeError) Unwrap() error { return e.Err }

// UsageError is a mistake on the command line; it exits with ExitUsage.
type UsageError struct{ msg string }

func (e UsageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return UsageError{fmt.Sprintf(format, a...)} }

func findCommand(name string) (command, bool) {
	for _, c := range commands {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

// HostFunc runs `host ACTION [ID]` against the server's database, which the
// client never opens, so the binary supplies it. add is the record for
// `host add`; id names the record for start, stop and remove.
type HostFunc func(action, id string, add wire.HostedSessionCreate, jsonOut bool, stdout io.Writer) (int, error)

// AdminFunc runs `admin ACTION NAME [--json]`. Admin users live in the
// server's database, which the client never opens, so the binary supplies it.
type AdminFunc func(action, name string, jsonOut bool, stdin io.Reader, stdout, stderr io.Writer) (int, error)

// UpFunc runs `up`: the server and a node in one process, until stopped.
type UpFunc func(dataDir string, port int, nodeName string) error

// NodeFunc runs `node`: a node attached to the host at hostURL, until
// stopped. A non-empty joinCode is first exchanged for the token written to
// tokenFile.
type NodeFunc func(hostURL, tokenFile, joinCode string, restartSessions bool) error

// NodeInviteFunc runs `node invite NAME` against the server's database.
type NodeInviteFunc func(name string, stdout io.Writer) (int, error)

// Commands are the commands the binary supplies because they run the server
// or open its database.
type Commands struct {
	Admin      AdminFunc
	Host       HostFunc
	Up         UpFunc
	Node       NodeFunc
	NodeInvite NodeInviteFunc
}

var (
	version string    // build version, from Run
	admin   AdminFunc // the admin command, from Run
	host    HostFunc  // the host command, from Run
	binary  Commands  // every command the binary supplies, from Run
)

// Run runs the client command in args (the arguments after the program name)
// and returns the process exit code.
func Run(args []string, buildVersion string, cmds Commands) int {
	version, admin, host, binary = buildVersion, cmds.Admin, cmds.Host, cmds
	return runCLI(args)
}

// Usagef reports a UsageError (exit code 2) from a command the binary supplies.
func Usagef(format string, a ...any) error { return usagef(format, a...) }

func runCLI(args []string) int {
	code, err := dispatch(args)
	var ue UsageError
	if errors.As(err, &ue) {
		fmt.Fprintln(os.Stderr, progName()+": "+ue.msg)
		fmt.Fprintln(os.Stderr, "Run '"+progName()+" --help' for usage.")
		return ExitUsage
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, progName()+": "+err.Error())
		var ce ExitCodeError
		if errors.As(err, &ce) {
			return ce.Code
		}
		return ExitError
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
			return ExitOK, nil
		case a == "--profile":
			if len(args) < 2 {
				return 0, usagef("--profile requires a value")
			}
			profileFlag, args = args[1], args[2:]
		case strings.HasPrefix(a, "--profile="):
			profileFlag, args = strings.TrimPrefix(a, "--profile="), args[1:]
		case a == "--role":
			if len(args) < 2 {
				return 0, usagef("--role requires a value")
			}
			roleFlag, args = args[1], args[2:]
		case strings.HasPrefix(a, "--role="):
			roleFlag, args = strings.TrimPrefix(a, "--role="), args[1:]
		default:
			return 0, usagef("unknown option %s", a)
		}
	}
	if len(args) == 0 {
		return 0, usagef("missing command")
	}
	if args[0] == runnerCommand {
		// Hidden: the detached job runner `job run` starts (not in help).
		if len(args) != 2 {
			return 0, usagef("%s takes a job ID", runnerCommand)
		}
		c, err := loadConfig()
		if err != nil {
			return 0, err
		}
		return jobRunner(c, args[1])
	}
	name, rest := args[0], args[1:]
	if name == "help" {
		if len(rest) == 0 {
			printHelp()
			return ExitOK, nil
		}
		if _, ok := findCommand(rest[0]); !ok {
			return 0, usagef("unknown command %q", rest[0])
		}
		printCommandHelp(rest[0])
		return ExitOK, nil
	}
	cmd, ok := findCommand(name)
	if !ok {
		return 0, usagef("unknown command %q", name)
	}
	if hasHelp(rest) || len(rest) > 0 && rest[0] == "help" {
		printCommandHelp(name)
		return ExitOK, nil
	}
	p, err := parseCommandArgs(cmd, rest)
	if err != nil {
		return 0, usagef("%s: %s (see '%s %s --help')", name, err.Error(), progName(), name)
	}
	if name == "host" {
		// Its --profile names the hosted session's profile, not this
		// client's credentials.
		return cliHost(p, os.Stdout)
	}
	if v, ok := p.values["--profile"]; ok {
		profileFlag = v
	}
	if profileFlag != "" && !validProfile(profileFlag) {
		return 0, usagef("invalid profile name %q", profileFlag)
	}
	if v, from := config.EnvAliasFrom(os.Getenv, "PROFILE"); profileFlag == "" && v != "" && !validProfile(v) {
		return 0, usagef("invalid profile name %q in %s", v, from)
	}
	switch name {
	case "version":
		fmt.Println(progName() + " " + version)
		return ExitOK, nil
	case "register":
		return cliRegister(p)
	case "admin":
		return admin(p.pos[0], p.pos[1], p.bool("--json"), os.Stdin, os.Stdout, os.Stderr)
	case "up":
		return cliUp(p)
	case "node":
		return cliNode(p, os.Stdout)
	case "join":
		return cliJoin(p, os.Stdout)
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
	case "invite":
		return cliInvite(c, p, os.Stdout)
	case "inbox", "outbox":
		return cliList(c, name, p)
	case "wait":
		return cliWait(c, p)
	case "watch":
		if len(p.pos) > 0 || len(p.extra) > 0 {
			return watchStart(c, p)
		}
		return cliWatch(c, p)
	case "events":
		return cliEvents(c, p)
	case "slots":
		return cliSlots(c, p)
	case "slot":
		return cliSlot(c, p)
	case "status":
		return cliStatus(c, p)
	case "metrics":
		return cliMetrics(c, p)
	case "decisions":
		return cliDecisions(c, p)
	case "progress":
		return cliProgress(c, p)
	case "report":
		return cliReport(c, p, os.Stdout)
	case "job":
		return cliJob(c, p)
	case "schedule":
		return cliSchedule(c, p)
	case "permission":
		return cliPermission(c, p)
	case "question":
		return cliQuestion(c, p)
	case "read", "ack", "complete":
		method, u := "GET", endpoint(c, "/api/messages/"+p.pos[0])
		if name != "read" {
			method, u = "POST", u+"/"+name
		}
		req, _ := http.NewRequest(method, u, nil)
		return doRequest(c, req, p.bool("--json"))
	case "close":
		if p.bool("--json") {
			req, _ := http.NewRequest("POST", endpoint(c, "/api/messages/"+p.pos[0]+"/close"), nil)
			return doRequest(c, req, true)
		}
		b, e := slotCall(c, "POST", "/api/messages/"+p.pos[0]+"/close", nil)
		if e != nil {
			return 0, e
		}
		var out wire.Closed
		if e = json.Unmarshal(b, &out); e != nil {
			return 0, fmt.Errorf("unexpected response from server: %v", e)
		}
		fmt.Printf("#%d closed\n", out.ID)
		return ExitOK, nil
	case "reply":
		body, e := inputBody(p)
		if e != nil {
			return 0, e
		}
		msg := withPriority(p, map[string]any{"subject": p.values["--subject"], "body": body, "recipient": p.values["--to"]})
		if p.bool("--directive") {
			// Omitted otherwise, like priority, so older servers accept the request.
			msg["directive"] = true
		}
		if p.bool("--done") {
			msg["done"] = true
		}
		b, _ := json.Marshal(msg)
		req, _ := http.NewRequest("POST", endpoint(c, "/api/messages/"+p.pos[0]+"/reply"), bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if key := p.values["--idempotency-key"]; key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		return cliReply(c, req, p.pos[0], p.bool("--json"))
	}
	return 0, usagef("unknown command %q", name)
}

type parsedArgs struct {
	values map[string]string
	multi  map[string][]string // every value of an option given more than once, in order
	pos    []string
	extra  []string // after --, for a command with extra
}

func (p parsedArgs) bool(name string) bool { _, ok := p.values[name]; return ok }

// parseCommandArgs accepts only the options declared for cmd. Unknown options,
// missing values and a wrong number of positional arguments are errors.
func parseCommandArgs(cmd command, args []string) (parsedArgs, error) {
	p := parsedArgs{values: map[string]string{}, multi: map[string][]string{}}
	known := map[string]option{}
	for _, o := range cmd.options {
		known[o.name] = o
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" && cmd.extra {
			p.extra = append([]string{}, args[i+1:]...)
			break
		}
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
		p.multi[name] = append(p.multi[name], value)
	}
	if len(p.pos) != cmd.nargs && !(cmd.optArgs && len(p.pos) < cmd.nargs) {
		if cmd.nargs == 0 {
			return p, fmt.Errorf("unexpected argument %q", p.pos[0])
		}
		return p, fmt.Errorf("expected %s, got %d argument(s)", cmd.args, len(p.pos))
	}
	return p, nil
}

func hasHelp(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

func printHelp() {
	var b strings.Builder
	b.WriteString("PROG — role addressed durable mailbox\n\nUsage: PROG [--profile NAME] [--role ROLE] <command> [options]\n\nCommands:\n")
	for _, c := range commands {
		label := c.name
		if c.args != "" {
			label += " " + c.args
		}
		fmt.Fprintf(&b, "  %-12s %s\n", label, c.summary)
	}
	b.WriteString(`  help CMD     Show options for a command

Examples:
  PROG register --endpoint https://mail.example --role team/worker --token-file role.token --enrollment-token-file enrollment.token
  PROG --profile worker config set --endpoint https://mail.example --role team/worker --token-file role.token
  PROG --profile worker send --to team/lead --subject 'Done' --body 'Work is complete'
  PROG inbox --status pending --json
  PROG wait --timeout 1800
  PROG events --after 41 --timeout 25

Use 'PROG <command> --help' for command options.
--profile NAME may come before or after the command; it overrides SWITCHBOARD_PROFILE.
A named profile that has no saved config is an error.
--role ROLE (before the command) or SWITCHBOARD_ROLE acts as a numbered role of the
profile's role, e.g. team/worker-2 for a team/worker profile (--role wins over the env).
Every SWITCHBOARD_* variable also answers to its old AGENT_MAIL_* name; the new name wins.

Exit codes: 0 success, 1 error, 2 usage error (unknown command or option), 3 wait or events found no mail.
`)
	fmt.Print(strings.ReplaceAll(b.String(), "PROG", progName()))
}

func printCommandHelp(name string) {
	c, ok := findCommand(name)
	if !ok {
		printHelp()
		return
	}
	usage := progName() + " " + c.name
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
	if d := config.EnvAlias("CONFIG_DIR"); d != "" {
		return filepath.Join(d, filename)
	}
	d, e := os.UserConfigDir()
	if e != nil {
		d = "."
	}
	return profilePath(d, filename)
}

// profilePath returns <config>/switchboard/<file>. When only the old
// <config>/<config.LegacyConfigDir>/ holds that profile, it is copied once into the new
// directory (0700 directory, 0600 file) and the old file is left in place;
// from then on only the new file is read and written. An existing new file
// is never overwritten. If the copy fails, the old file is used as is.
func profilePath(configDir, filename string) string {
	p := filepath.Join(configDir, config.ConfigDirName, filename)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	old := filepath.Join(configDir, config.LegacyConfigDir, filename)
	if _, err := os.Stat(old); err != nil {
		return p
	}
	if copyProfile(old, p) != nil {
		return old
	}
	return p
}

// copyProfile copies src to dst, owner-only, failing if dst exists.
func copyProfile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	if err = ownerfile.RestrictToUser(dst); err != nil {
		f.Close()
		os.Remove(dst)
		return err
	}
	if _, err = f.Write(b); err != nil {
		f.Close()
		os.Remove(dst)
		return err
	}
	if err = f.Close(); err != nil {
		os.Remove(dst)
	}
	return err
}

// explicitProfile reports whether the user named a profile (flag or environment).
func explicitProfile() bool { return profileName() != "default" }

func profileName() string {
	name := profileFlag
	if name == "" {
		name = config.EnvAlias("PROFILE")
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
	if v := config.EnvAlias("ENDPOINT"); v != "" {
		c.Endpoint = v
	}
	// With a saved role and no SWITCHBOARD_TOKEN, SWITCHBOARD_ROLE picks a
	// numbered role of the saved one (see actingRole); otherwise it is the
	// role itself, as in environment-only setups.
	if v := config.EnvAlias("ROLE"); v != "" && (c.Role == "" || config.EnvAlias("TOKEN") != "") {
		c.Role = v
	}
	if v := config.EnvAlias("TOKEN"); v != "" {
		c.Token = v
	}
	return c
}

// loadConfig returns the active profile's configuration. A profile named on the
// command line or in SWITCHBOARD_PROFILE must exist; it never falls back to another profile.
func loadConfig() (Config, error) {
	p := configPath()
	if explicitProfile() {
		if _, err := os.Stat(p); err != nil {
			return Config{}, fmt.Errorf("profile %q not found (%s); create it with: %s --profile %s config set --endpoint URL --role ROLE --token-file PATH", profileName(), p, progName(), profileName())
		}
	}
	c := readConfig()
	if c.Endpoint == "" || c.Token == "" || c.Role == "" {
		return c, fmt.Errorf("profile %q is not configured (%s); run: %s config set --endpoint URL --role ROLE --token-file PATH", profileName(), p, progName())
	}
	role, err := actingRole(c.Role)
	if err != nil {
		return c, err
	}
	c.Role = role
	return c, nil
}

// roleFlag holds the global --role value. It wins over SWITCHBOARD_ROLE.
var roleFlag string

// actingRole is the X-Switchboard-Role to send: --role, else SWITCHBOARD_ROLE,
// else the profile's role. An override must be the profile role itself or a
// numbered role <base>-N under it, since the profile's token covers only those.
func actingRole(profileRole string) (string, error) {
	want, from := roleFlag, "--role"
	if want == "" && config.EnvAlias("TOKEN") == "" {
		want, from = config.EnvAliasFrom(os.Getenv, "ROLE")
	}
	if want == "" || want == profileRole {
		return profileRole, nil
	}
	base, _, ok := wire.SplitSlot(want)
	if ok {
		if pb, _, numbered := wire.SplitSlot(profileRole); base == profileRole || numbered && base == pb {
			return want, nil
		}
	}
	return "", usagef("%s %q must be %s or %s-N (the profile's role or one of its numbered roles)", from, want, profileRole, profileRole)
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
	if ownerfile.Write(path, b) != nil {
		return 0, errors.New("cannot write config")
	}
	fmt.Println("Saved", path)
	return ExitOK, nil
}

func cliRegister(p parsedArgs) (int, error) {
	for _, k := range []string{"--endpoint", "--role", "--token-file", "--enrollment-token-file"} {
		if p.values[k] == "" {
			return 0, usagef("register requires --endpoint, --role, --token-file and --enrollment-token-file")
		}
	}
	if !wire.ValidRole(p.values["--role"]) {
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
	return ExitOK, nil
}

// cliReply sends a reply and prints it like doRequest. A reply that leaves
// its directive open (no --done) adds one line on stderr saying so (#269).
func cliReply(c Config, req *http.Request, id string, jsonMode bool) (int, error) {
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
	var out wire.Message
	if json.Unmarshal(b, &out) == nil && out.DirectiveState == "open" {
		fmt.Fprintln(os.Stderr, ReplyOpenNote(id))
	}
	return printResponse(b, jsonMode), nil
}

// DirectiveOpenNote is the line read prints under the header of an answer
// to a directive the reader sent that is still open (#275).
func DirectiveOpenNote(id int64) string {
	return fmt.Sprintf("directive #%d is still open: close %d when it is done, or reply to ask for more", id, id)
}

// ReplyOpenNote is the stderr line of a reply that leaves its directive open.
func ReplyOpenNote(id string) string {
	return "directive #" + id + " stays open; reply " + id + " --done (or complete " + id + ") closes it"
}

func endpoint(c Config, path string) string { return strings.TrimRight(c.Endpoint, "/") + path }

func cliSend(c Config, p parsedArgs) (int, error) {
	body, e := inputBody(p)
	if e != nil {
		return 0, e
	}
	b, _ := json.Marshal(withPriority(p, map[string]any{"recipient": p.values["--to"], "subject": p.values["--subject"], "body": body, "task_id": p.values["--task"]}))
	req, _ := http.NewRequest("POST", endpoint(c, "/api/messages"), bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if key := p.values["--idempotency-key"]; key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	return doRequestWith(c, req, p.bool("--json"), noSessionNote(p.values["--to"]))
}

// noSessionNote returns a hook for the send response: one line on stderr when
// the server says no session of the recipient role is attached (#300).
func noSessionNote(to string) func([]byte) {
	return func(b []byte) {
		var m wire.Message
		if json.Unmarshal(b, &m) == nil && m.NoSession {
			fmt.Fprintf(os.Stderr, "no session of %s is attached; the mail waits in its inbox\n", to)
		}
	}
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

// withPriority adds "priority":"urgent" for --urgent and "no_reply":true for
// --no-reply. Without the flags the fields are omitted, so servers that
// predate them accept the request unchanged.
func withPriority(p parsedArgs, req map[string]any) map[string]any {
	if p.bool("--urgent") {
		req["priority"] = "urgent"
	}
	if p.bool("--no-reply") {
		// Omitted otherwise, so older servers accept the request unchanged.
		req["no_reply"] = true
	}
	return req
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
	req.Header.Set(config.HeaderPrefix+"Role", c.Role)
}

func doRequest(c Config, req *http.Request, jsonMode bool) (int, error) {
	return doRequestWith(c, req, jsonMode, nil)
}

// doRequestWith is doRequest that also hands the response body to after.
func doRequestWith(c Config, req *http.Request, jsonMode bool, after func([]byte)) (int, error) {
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
	if after != nil {
		after(b)
	}
	return printResponse(b, jsonMode), nil
}

// printResponse prints a response body: indented JSON with jsonMode, else
// for people.
func printResponse(b []byte, jsonMode bool) int {
	if jsonMode {
		var out bytes.Buffer
		if json.Indent(&out, b, "", "  ") == nil {
			b = out.Bytes()
		}
		fmt.Println(string(b))
		return ExitOK
	}
	var v any
	if json.Unmarshal(b, &v) == nil {
		pretty(v)
	} else {
		fmt.Println(string(b))
	}
	return ExitOK
}

func pretty(v any) { prettyTo(os.Stdout, v) }

// prettyTo prints messages for people. A header line starts with "#<id>";
// every body line is indented by two spaces, so a body line that itself
// starts with "#123" never reads as a header.
func prettyTo(w io.Writer, v any) {
	switch x := v.(type) {
	case []any:
		for _, y := range x {
			prettyTo(w, y)
			fmt.Fprintln(w)
		}
	case map[string]any:
		mark := ""
		if x["priority"] == "urgent" {
			mark = "[urgent] "
		}
		body, _ := x["body"].(string)
		fmt.Fprintf(w, "#%v %s%v\n%v → %v · %s\n", x["id"], mark, x["subject"], x["sender"], x["recipient"], x["status"])
		if ref, ok := x["answers_directive"].(map[string]any); ok && ref["state"] == wire.DirectiveOpen {
			if id, ok := ref["id"].(float64); ok {
				fmt.Fprintln(w, DirectiveOpenNote(int64(id)))
			}
		}
		fmt.Fprintf(w, "%s\n", indentBody(body))
	default:
		fmt.Fprintln(w, v)
	}
}

// indentBody prefixes every line of body with two spaces.
func indentBody(body string) string {
	return "  " + strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\n  ")
}

func secondsOption(cmd string, p parsedArgs, name string, def int, min int) (time.Duration, error) {
	raw, ok := p.values[name]
	if !ok {
		return time.Duration(def) * time.Second, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < min {
		return 0, usagef("%s: %s must be a whole number of seconds >= %d", cmd, name, min)
	}
	return time.Duration(n) * time.Second, nil
}

// errPermanent marks request failures that retrying cannot fix (bad credentials, bad request).
type errPermanent struct{ error }

func fetchPending(client *http.Client, c Config) ([]wire.Message, error) {
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
	var all []wire.Message
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, fmt.Errorf("unexpected response from server: %v", err)
	}
	pending := []wire.Message{}
	for _, m := range all {
		// Older servers ignore the status filter; filter here as well.
		if m.Status == "pending" {
			if m.Priority == "" { // servers that predate message priority
				m.Priority = "normal"
			}
			pending = append(pending, m)
		}
	}
	return pending, nil
}

// cliWait blocks until the active role has pending inbox messages, prints them and
// exits 0. It never changes message state. With no pending mail before the timeout it exits 3.
func cliWait(c Config, p parsedArgs) (int, error) {
	timeout, err := secondsOption("wait", p, "--timeout", 1800, 0)
	if err != nil {
		return 0, err
	}
	interval, err := secondsOption("wait", p, "--interval", 15, 1)
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
					fmt.Println(mailLine(m.ID, m.Sender, m.Subject, m.TaskID, m.Priority))
				}
			}
			return ExitOK, nil
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
				fmt.Fprintf(os.Stderr, progName()+" wait: no pending messages after %s\n", timeout)
				return ExitTimeout, nil
			}
			if left < sleep {
				sleep = left
			}
		}
		time.Sleep(sleep)
	}
}

// mailLine is the one-line summary printed by wait and events:
// "#ID sender | subject[ | task=ID][ | urgent]".
func mailLine(id int64, sender, subject, task, priority string) string {
	line := fmt.Sprintf("#%d %s | %s", id, sender, oneLine(subject))
	if task != "" {
		line += " | task=" + task
	}
	if priority == "urgent" {
		line += " | urgent"
	}
	return line
}

// cliEvents makes one long-poll request to /api/events/poll. It exits 0 when
// events came back, 3 when the wait ended with none. It never changes message
// state; pass the printed cursor back with --after to resume.
func cliEvents(c Config, p parsedArgs) (int, error) {
	q := url.Values{}
	if raw, ok := p.values["--after"]; ok {
		if id, e := strconv.ParseInt(raw, 10, 64); e != nil || id < 0 {
			return 0, usagef("events: --after must be a message ID >= 0")
		}
		q.Set("after", raw)
	}
	wait, err := secondsOption("events", p, "--timeout", 25, 0)
	if err != nil {
		return 0, err
	}
	if _, ok := p.values["--timeout"]; ok {
		q.Set("timeout", strconv.FormatInt(int64(wait/time.Second), 10))
	}
	u := endpoint(c, "/api/events/poll")
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, _ := http.NewRequest("GET", u, nil)
	authorize(c, req)
	resp, err := (&http.Client{Timeout: wait + 30*time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("server returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var v struct {
		Events []wire.MailEvent `json:"events"`
		Cursor int64            `json:"cursor"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return 0, fmt.Errorf("unexpected response from server: %v", err)
	}
	if p.bool("--json") {
		fmt.Println(strings.TrimSpace(string(b)))
	} else {
		for _, ev := range v.Events {
			fmt.Println(mailLine(ev.ID, ev.Sender, ev.Subject, ev.TaskID, ev.Priority))
		}
	}
	if len(v.Events) == 0 {
		fmt.Fprintf(os.Stderr, progName()+" events: no new mail (cursor %d)\n", v.Cursor)
		return ExitTimeout, nil
	}
	return ExitOK, nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func cliWatch(c Config, p parsedArgs) (int, error) {
	after, _ := wire.ParseID(p.values["--after"])
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
			Messages []int64                      `json:"messages"`
			Cursor   int64                        `json:"cursor"`
			Answers  map[string]wire.DirectiveRef `json:"answers_directive"`
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
			ref, answers := v.Answers[strconv.FormatInt(id, 10)]
			if jsonMode {
				line := struct {
					MessageID int64              `json:"message_id"`
					Cursor    int64              `json:"cursor"`
					Answers   *wire.DirectiveRef `json:"answers_directive,omitempty"`
				}{MessageID: id, Cursor: after}
				if answers {
					line.Answers = &ref
				}
				b, _ := json.Marshal(line)
				fmt.Println(string(b))
			} else if answers && ref.State == wire.DirectiveOpen {
				fmt.Printf("New message #%d (cursor %d): answers directive #%d, still open\n", id, after, ref.ID)
			} else {
				fmt.Printf("New message #%d (cursor %d)\n", id, after)
			}
		}
	}
}

func fatal(s string) { fmt.Fprintln(os.Stderr, s); os.Exit(ExitError) }

// slotCall sends one slot API request and returns the response body.
func slotCall(c Config, method, path string, body any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, endpoint(c, path), rd)
	req.Header.Set("Content-Type", "application/json")
	authorize(c, req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("server returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return b, nil
}

func slotLine(v wire.SlotView) string {
	line := fmt.Sprintf("%s  %s:%s  until %s", v.Role, v.Machine, v.Directory, v.LeaseExpiresAt.Format(time.RFC3339))
	if v.Label != "" {
		line += "  label=" + v.Label
	}
	if v.Parent != "" {
		line += "  parent=" + v.Parent
	}
	return line
}

func cliSlots(c Config, p parsedArgs) (int, error) {
	b, err := slotCall(c, "GET", "/api/slots", nil)
	if err != nil {
		return 0, err
	}
	var out []wire.SlotView
	if err = json.Unmarshal(b, &out); err != nil {
		return 0, fmt.Errorf("unexpected response from server: %v", err)
	}
	if p.bool("--json") {
		fmt.Println(strings.TrimSpace(string(b)))
		return ExitOK, nil
	}
	for _, v := range out {
		fmt.Println(slotLine(v))
	}
	return ExitOK, nil
}

func cliSlot(c Config, p parsedArgs) (int, error) {
	action := p.pos[0]
	switch action {
	case "register", "heartbeat", "takeover", "release":
	default:
		return 0, usagef("slot: unknown action %q (register, heartbeat, takeover, release)", action)
	}
	req := wire.SlotRequest{Machine: p.values["--machine"], Directory: p.values["--directory"], SessionID: p.values["--session-id"], Label: p.values["--label"], Parent: p.values["--parent"],
		State: p.values["--state"], LastTurnAt: p.values["--last-turn-at"]}
	if v, ok := p.values["--context-tokens"]; ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, usagef("slot: --context-tokens must be a whole number of at least 0")
		}
		req.ContextTokens = &n
	}
	if v, ok := p.values["--subagents"]; ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, usagef("slot: --subagents must be a whole number of at least 0")
		}
		req.Subagents = &n
	}
	if _, err := req.Presence(); err != nil {
		return 0, usagef("slot: %s", err.Error())
	}
	if req.Machine == "" {
		req.Machine, _ = os.Hostname()
	}
	if req.Directory == "" {
		req.Directory, _ = os.Getwd()
	}
	b, err := slotCall(c, "POST", "/api/slots/"+action, req)
	if err != nil {
		return 0, err
	}
	var g wire.SlotGrant
	if err = json.Unmarshal(b, &g); err != nil {
		return 0, fmt.Errorf("unexpected response from server: %v", err)
	}
	if p.bool("--json") {
		fmt.Println(strings.TrimSpace(string(b)))
		return ExitOK, nil
	}
	fmt.Printf("%s (%d other active)\n", slotLine(g.SlotView), g.OtherActive)
	if g.Displaced != nil {
		fmt.Printf("moved previous holder: %s\n", slotLine(*g.Displaced))
	}
	return ExitOK, nil
}

// progName is the name the CLI was invoked as, for help and messages:
// the old name (config.LegacyConfigDir) when run under it, otherwise switchboard.
func progName() string {
	base := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if base == config.LegacyConfigDir {
		return config.LegacyConfigDir
	}
	return "switchboard"
}
