package insight

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

	"github.com/sandcorporation/agent-mail/internal/config"
)

const discordNoticeTimeout = 5 * time.Second

// Discord posts a one-line notice for each new question to a Discord
// webhook. The webhook URL is a credential: it is never logged or put into an
// error message. A nil notifier sends nothing.
type Discord struct {
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

// ConfigureDiscord reads the SWITCHBOARD_DISCORD_* and SWITCHBOARD_PUBLIC_URL
// variables (or their AGENT_MAIL_* aliases). An unset or empty webhook returns nil (no notices).
func ConfigureDiscord(getenv func(string) string) *Discord {
	get := func(name string) (string, string) {
		v, from := config.EnvAliasFrom(getenv, name)
		return strings.TrimSpace(v), from
	}
	webhook, _ := get("DISCORD_WEBHOOK")
	if webhook == "" {
		return nil
	}
	avatar, avatarFrom := get("DISCORD_AVATAR_URL")
	if avatar != "" && !strings.HasPrefix(avatar, "https://") {
		log.Printf("%s must start with https://; ignored", avatarFrom)
		avatar = ""
	}
	publicURL, _ := get("PUBLIC_URL")
	username, _ := get("DISCORD_USERNAME")
	return &Discord{
		webhook:   webhook,
		publicURL: strings.TrimRight(publicURL, "/"),
		username:  username,
		avatarURL: avatar,
		timeout:   discordNoticeTimeout,
		client:    &http.Client{},
	}
}

// questionNoticeLine is the notice text: ID, asker and first header only,
// never the question text or options.
func (d *Discord) questionNoticeLine(id, asker, header string) string {
	header = strings.Join(strings.Fields(header), " ")
	line := fmt.Sprintf("질문 %s · %s · %s", id, asker, header)
	if d.publicURL != "" {
		line += " " + d.publicURL + "/#q=" + id
	}
	return line
}

// NoticeQuestion sends the notice in the background. Call it only after the
// question's transaction has committed; it never blocks the caller.
func (d *Discord) NoticeQuestion(id, asker, header string) {
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
func (d *Discord) post(body []byte) string {
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

// Wait blocks until every notice sent so far has finished (tests).
func (d *Discord) Wait() {
	if d != nil {
		d.pending.Wait()
	}
}

// AvatarURL is the avatar the notices use; "" when unset or ignored (tests).
func (d *Discord) AvatarURL() string { return d.avatarURL }

// SetTimeout changes how long one notice may take (tests).
func (d *Discord) SetTimeout(t time.Duration) { d.timeout = t }
