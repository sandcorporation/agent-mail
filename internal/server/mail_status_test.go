package server

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// statusViews returns the status of message id as each view shows it: the
// sender's read and outbox, the recipient's inbox and (last, since it marks
// pending mail read) the recipient's read.
func statusViews(t *testing.T, h *cliHarness, id int64, recipientRead bool) map[string]string {
	t.Helper()
	one := func(args ...string) string {
		var m wire.Message
		if err := json.Unmarshal([]byte(h.mustRun(nil, args...)), &m); err != nil {
			t.Fatal(err)
		}
		return m.Status
	}
	box := func(args ...string) string {
		var ms []wire.Message
		if err := json.Unmarshal([]byte(h.mustRun(nil, args...)), &ms); err != nil {
			t.Fatal(err)
		}
		for _, m := range ms {
			if m.ID == id {
				return m.Status
			}
		}
		t.Fatalf("%v: message %d missing", args, id)
		return ""
	}
	n := strconv.FormatInt(id, 10)
	got := map[string]string{
		"sender read":     one("--profile", "a", "read", n, "--json"),
		"sender outbox":   box("--profile", "a", "outbox", "--json"),
		"recipient inbox": box("--profile", "b", "inbox", "--json"),
	}
	if recipientRead {
		got["recipient read"] = one("--profile", "b", "read", n, "--json")
	}
	return got
}

func sendID(t *testing.T, h *cliHarness, subject, body string) int64 {
	t.Helper()
	var m wire.Message
	if err := json.Unmarshal([]byte(h.mustRun(nil, "--profile", "a", "send", "--to", "b", "--subject", subject, "--body", body, "--json")), &m); err != nil {
		t.Fatal(err)
	}
	return m.ID
}

func wantAll(t *testing.T, name string, got map[string]string, want string) {
	t.Helper()
	for view, s := range got {
		if s != want {
			t.Errorf("%s: %s shows %q, want %q (all: %v)", name, view, s, want, got)
		}
	}
}

// Regression: the sender's read showed messages.status (pending) while outbox
// showed the recipient's state from role_states.
func TestSenderAndRecipientSeeOneStatus(t *testing.T) {
	h := newCLIHarness(t)
	h.configure([]string{"--profile", "a"}, "a")
	h.configure([]string{"--profile", "b"}, "b")

	done := sendID(t, h, "done", "x")
	h.mustRun(nil, "--profile", "b", "complete", strconv.FormatInt(done, 10))
	wantAll(t, "complete", statusViews(t, h, done, true), "complete")

	seen := sendID(t, h, "seen", "x")
	h.mustRun(nil, "--profile", "b", "read", strconv.FormatInt(seen, 10))
	wantAll(t, "read", statusViews(t, h, seen, true), "read")

	// The sender's read changes nothing: everyone still sees pending.
	fresh := sendID(t, h, "fresh", "x")
	h.mustRun(nil, "--profile", "a", "read", strconv.FormatInt(fresh, 10))
	wantAll(t, "sender read", statusViews(t, h, fresh, false), "pending")
	// The recipient's read still marks it read, for everyone.
	h.mustRun(nil, "--profile", "b", "read", strconv.FormatInt(fresh, 10))
	wantAll(t, "recipient read", statusViews(t, h, fresh, true), "read")
}

// Regression: a body line starting with "#123" read like a message header in
// human output. Body lines are indented; only headers match ^#\d+.
func TestHumanOutputBodyNeverLooksLikeHeader(t *testing.T) {
	h := newCLIHarness(t)
	h.configure([]string{"--profile", "a"}, "a")
	h.configure([]string{"--profile", "b"}, "b")
	first := sendID(t, h, "one", "#123 fake\nsecond line\n#7")
	second := sendID(t, h, "two", "plain")
	header := regexp.MustCompile(`^#\d+`)
	headers := func(out string) []string {
		var hs []string
		for _, line := range strings.Split(out, "\n") {
			if header.MatchString(line) {
				hs = append(hs, header.FindString(line))
			}
		}
		return hs
	}
	for _, args := range [][]string{{"--profile", "b", "inbox"}, {"--profile", "a", "outbox"}} {
		out := h.mustRun(nil, args...)
		got := strings.Join(headers(out), ",")
		if want := "#" + strconv.FormatInt(second, 10) + ",#" + strconv.FormatInt(first, 10); got != want {
			t.Fatalf("%v headers %s, want %s\n%s", args, got, want, out)
		}
		if !strings.Contains(out, "\n  #123 fake\n  second line\n  #7") {
			t.Fatalf("%v body not indented:\n%s", args, out)
		}
	}
	out := h.mustRun(nil, "--profile", "b", "read", strconv.FormatInt(first, 10))
	if got := headers(out); len(got) != 1 || got[0] != "#"+strconv.FormatInt(first, 10) {
		t.Fatalf("read headers %v\n%s", got, out)
	}
	// --json is unchanged: the body is stored and returned as sent.
	var m wire.Message
	if err := json.Unmarshal([]byte(h.mustRun(nil, "--profile", "b", "read", strconv.FormatInt(first, 10), "--json")), &m); err != nil || m.Body != "#123 fake\nsecond line\n#7" {
		t.Fatalf("json body %q %v", m.Body, err)
	}
}
