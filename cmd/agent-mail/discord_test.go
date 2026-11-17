package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// webhookRecorder is an httptest stand-in for a Discord webhook.
type webhookRecorder struct {
	mu     sync.Mutex
	bodies [][]byte
	status int           // 0 means 204, like Discord
	hang   chan struct{} // non-nil: block until closed or the request is cancelled
	srv    *httptest.Server
}

func newWebhook(t *testing.T) *webhookRecorder {
	t.Helper()
	h := &webhookRecorder{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.bodies = append(h.bodies, b)
		status, hang := h.status, h.hang
		h.mu.Unlock()
		if hang != nil {
			select {
			case <-hang:
			case <-r.Context().Done():
			}
		}
		if status == 0 {
			status = http.StatusNoContent
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(func() {
		h.mu.Lock()
		if h.hang != nil {
			close(h.hang)
			h.hang = nil
		}
		h.mu.Unlock()
		h.srv.Close()
	})
	return h
}

func (h *webhookRecorder) received() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]byte(nil), h.bodies...)
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// discordQuestionServer: questionServer with discord on for every role and the
// notifier pointed at hook.
func discordQuestionServer(t *testing.T, hook *webhookRecorder, extra map[string]string) *Server {
	t.Helper()
	s := questionServer(t)
	if _, err := s.db.Exec(`UPDATE question_settings SET discord=1 WHERE target='default'`); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"AGENT_MAIL_DISCORD_WEBHOOK": hook.srv.URL + "/api/webhooks/1/secret"}
	for k, v := range extra {
		env[k] = v
	}
	configureDiscord(s, envOf(env))
	return s
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func TestDiscordNotSentWithoutWebhook(t *testing.T) {
	hook := newWebhook(t)
	s := discordQuestionServer(t, hook, nil)
	configureDiscord(s, envOf(map[string]string{"AGENT_MAIL_PUBLIC_URL": "https://mail.example"}))
	if s.discord != nil {
		t.Fatal("notifier configured without a webhook")
	}
	askQuestionAs(t, s, workerToken, "team/worker-2", 201)
	s.discord.wait()
	if got := hook.received(); len(got) != 0 {
		t.Fatalf("sent without webhook: %s", got)
	}
}

func TestDiscordNotSentWhenSettingOff(t *testing.T) {
	hook := newWebhook(t)
	s := discordQuestionServer(t, hook, nil)
	if _, err := s.db.Exec(`UPDATE question_settings SET discord=0 WHERE target='default'`); err != nil {
		t.Fatal(err)
	}
	askQuestionAs(t, s, workerToken, "team/worker-2", 201)
	s.discord.wait()
	if got := hook.received(); len(got) != 0 {
		t.Fatalf("sent with discord=false: %s", got)
	}
}

func TestDiscordSentOnceWithoutQuestionText(t *testing.T) {
	hook := newWebhook(t)
	s := discordQuestionServer(t, hook, map[string]string{"AGENT_MAIL_PUBLIC_URL": "https://mail.example/"})
	v := askQuestionAs(t, s, workerToken, "team/worker-2", 201)
	s.discord.wait()
	got := hook.received()
	if len(got) != 1 {
		t.Fatalf("want one notice, got %d", len(got))
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(got[0], &raw); err != nil {
		t.Fatalf("%v: %s", err, got[0])
	}
	var content string
	_ = json.Unmarshal(raw["content"], &content)
	want := "질문 " + v.ID + " · team/worker-2 · 배포 https://mail.example/#q=" + v.ID
	if content != want {
		t.Fatalf("content %q, want %q", content, want)
	}
	if string(raw["allowed_mentions"]) != `{"parse":[]}` {
		t.Fatalf("allowed_mentions %s", raw["allowed_mentions"])
	}
	if _, ok := raw["username"]; ok {
		t.Fatalf("username sent while unset: %s", got[0])
	}
	if _, ok := raw["avatar_url"]; ok {
		t.Fatalf("avatar_url sent while unset: %s", got[0])
	}
	for _, secret := range []string{"지금 배포할까요?", "어디에?", "아니오", "내일", "worker: k"} {
		if strings.Contains(string(got[0]), secret) {
			t.Fatalf("notice leaks %q: %s", secret, got[0])
		}
	}
}

func TestDiscordLinkOmittedWithoutPublicURL(t *testing.T) {
	hook := newWebhook(t)
	s := discordQuestionServer(t, hook, nil)
	v := askQuestionAs(t, s, workerToken, "team/worker-2", 201)
	s.discord.wait()
	got := hook.received()
	if len(got) != 1 || !strings.Contains(string(got[0]), `"content":"질문 `+v.ID+` · team/worker-2 · 배포"`) {
		t.Fatalf("notice: %s", got)
	}
}

func TestDiscordUsernameAndAvatar(t *testing.T) {
	hook := newWebhook(t)
	s := discordQuestionServer(t, hook, map[string]string{
		"AGENT_MAIL_DISCORD_USERNAME":   "agent-mail",
		"AGENT_MAIL_DISCORD_AVATAR_URL": "https://mail.example/avatar.png",
	})
	askQuestionAs(t, s, workerToken, "team/worker-2", 201)
	s.discord.wait()
	got := hook.received()
	var p discordPayload
	if len(got) != 1 || json.Unmarshal(got[0], &p) != nil {
		t.Fatalf("notice: %s", got)
	}
	if p.Username != "agent-mail" || p.AvatarURL != "https://mail.example/avatar.png" {
		t.Fatalf("payload %+v", p)
	}
}

func TestDiscordAvatarMustBeHTTPS(t *testing.T) {
	hook := newWebhook(t)
	logs := captureLog(t)
	s := discordQuestionServer(t, hook, map[string]string{"AGENT_MAIL_DISCORD_AVATAR_URL": "http://mail.example/avatar.png"})
	if s.discord.avatarURL != "" {
		t.Fatalf("non-https avatar kept")
	}
	if !strings.Contains(logs.String(), "AGENT_MAIL_DISCORD_AVATAR_URL must start with https://") || strings.Contains(logs.String(), "mail.example") {
		t.Fatalf("log: %q", logs.String())
	}
}

func TestDiscordFailureDoesNotAffectAsk(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(h *webhookRecorder)
		want string
	}{
		{"500", func(h *webhookRecorder) { h.status = 500 }, "discord notice failed: status 500"},
		{"hang", func(h *webhookRecorder) { h.hang = make(chan struct{}) }, "discord notice failed: timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook := newWebhook(t)
			tc.set(hook)
			logs := captureLog(t)
			s := discordQuestionServer(t, hook, nil)
			s.discord.timeout = 1500 * time.Millisecond
			start := time.Now()
			askQuestionAs(t, s, workerToken, "team/worker-2", 201)
			if d := time.Since(start); d > 750*time.Millisecond {
				t.Fatalf("ask took %s", d)
			}
			s.discord.wait()
			out := logs.String()
			if !strings.Contains(out, tc.want) {
				t.Fatalf("log %q, want %q", out, tc.want)
			}
			if strings.Contains(out, hook.srv.URL) || strings.Contains(out, "secret") || strings.Contains(out, "127.0.0.1") {
				t.Fatalf("log leaks the webhook: %q", out)
			}
		})
	}
}
