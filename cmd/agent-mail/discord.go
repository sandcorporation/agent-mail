package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const discordNoticeTimeout = 5 * time.Second

// discordNotifier posts a one-line notice for each new question to a Discord
// webhook. The webhook URL is a credential: it is never logged or put into an
// error message. A nil notifier sends nothing.
type discordNotifier struct {
	webhook   string
	publicURL string // dashboard base URL without a trailing slash; "" omits the link
	username  string
	avatarURL string
	timeout   time.Duration
	client    *http.Client
	pending   sync.WaitGroup // notices in flight; tests wait on it
}

type discordMentions struct {
	Parse []string `json:"parse"`
}

type discordPayload struct {
	Content         string          `json:"content"`
	Username        string          `json:"username,omitempty"`
	AvatarURL       string          `json:"avatar_url,omitempty"`
	AllowedMentions discordMentions `json:"allowed_mentions"`
}

// configureDiscord reads the AGENT_MAIL_DISCORD_* and AGENT_MAIL_PUBLIC_URL
// variables. An unset or empty webhook leaves s.discord nil (no notices).
func configureDiscord(s *Server, getenv func(string) string) {
	webhook := strings.TrimSpace(getenv("AGENT_MAIL_DISCORD_WEBHOOK"))
	if webhook == "" {
		s.discord = nil
		return
	}
	avatar := strings.TrimSpace(getenv("AGENT_MAIL_DISCORD_AVATAR_URL"))
	if avatar != "" && !strings.HasPrefix(avatar, "https://") {
		log.Printf("AGENT_MAIL_DISCORD_AVATAR_URL must start with https://; ignored")
		avatar = ""
	}
	s.discord = &discordNotifier{
		webhook:   webhook,
		publicURL: strings.TrimRight(strings.TrimSpace(getenv("AGENT_MAIL_PUBLIC_URL")), "/"),
		username:  strings.TrimSpace(getenv("AGENT_MAIL_DISCORD_USERNAME")),
		avatarURL: avatar,
		timeout:   discordNoticeTimeout,
		client:    &http.Client{},
	}
}

// questionNoticeLine is the notice text: ID, asker and first header only,
// never the question text or options.
func (d *discordNotifier) questionNoticeLine(id, asker, header string) string {
	header = strings.Join(strings.Fields(header), " ")
	line := fmt.Sprintf("질문 %s · %s · %s", id, asker, header)
	if d.publicURL != "" {
		line += " " + d.publicURL + "/#q=" + id
	}
	return line
}

// noticeQuestion sends the notice in the background. Call it only after the
// question's transaction has committed; it never blocks the caller.
func (d *discordNotifier) noticeQuestion(id, asker, header string) {
	if d == nil {
		return
	}
	body, err := json.Marshal(discordPayload{
		Content:         d.questionNoticeLine(id, asker, header),
		Username:        d.username,
		AvatarURL:       d.avatarURL,
		AllowedMentions: discordMentions{Parse: []string{}},
	})
	if err != nil {
		log.Printf("discord notice failed: encode")
		return
	}
	d.pending.Add(1)
	go func() {
		defer d.pending.Done()
		if failure := d.post(body); failure != "" {
			log.Printf("discord notice failed: %s", failure)
		}
	}()
}

// post returns "" on success, else a status code or error class that does not
// contain the webhook URL.
func (d *discordNotifier) post(body []byte) string {
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.webhook, bytes.NewReader(body))
	if err != nil {
		return "invalid webhook"
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			return "timeout"
		}
		return "request error"
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Sprintf("status %d", resp.StatusCode)
	}
	return ""
}

// wait blocks until every notice sent so far has finished (tests).
func (d *discordNotifier) wait() {
	if d != nil {
		d.pending.Wait()
	}
}
