package hosting

import (
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// Notify sends one system mail (from switchboard/system) to each role in
// to, made idempotent by key: a second call with the same key sends nothing.
// The server passes it in; hosting does not import mail.
type Notify func(to []string, subject, body, key string) error

// SetNotify installs the mail function for node notices and the roles told
// about a record that has no parent (SWITCHBOARD_BOSS_ROLES). Without it a
// notice is only logged.
func (h *Hub) SetNotify(fn Notify, bosses []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notify, h.bosses = fn, append([]string(nil), bosses...)
}

// NoticeKey is the idempotency key of the mail for a notice:
// hosting:<kind>:<record id>:<run>.
func NoticeKey(n wire.NodeNotice) string {
	return "hosting:" + n.Kind + ":" + n.ID + ":" + strconv.Itoa(n.Run)
}

// notice acts on a node notice: a resume failure gets the record a new
// conversation (once: only while it still holds the failed one), then one
// mail goes to the record's parent, or to every boss role without one.
func (h *Hub) notice(n wire.NodeNotice) {
	rec, err := h.svc.Get(n.ID)
	if err != nil {
		log.Printf("hosting: notice for %s: %v", n.ID, err)
		return
	}
	var subject, body string
	switch n.Kind {
	case wire.NoticeCrashLoop:
		subject = "crash loop: " + who(rec)
		body = fmt.Sprintf("Hosted session %s %s exited %d times within 30 minutes, each run shorter than 10 minutes (last exit code %d, run %d). Its node makes no more attempts until Start or Restart.",
			rec.ID, where(rec), n.Runs, n.LastExit, n.Run)
	case wire.NoticeResumeFailed:
		replaced, changed, err := h.svc.replaceConversation(n.ID, n.AgentSessionID)
		if err != nil {
			log.Printf("hosting: new conversation for %s: %v", n.ID, err)
			return
		}
		if changed {
			if err := h.Sync(); err != nil {
				log.Printf("hosting: sync: %v", err)
			}
		}
		subject = "resume failed: " + who(rec)
		body = fmt.Sprintf("Hosted session %s %s could not resume conversation %s: %d runs with --resume in a row exited within 15 seconds (last exit code %d). It continues in a new conversation %s.",
			rec.ID, where(rec), n.AgentSessionID, n.Runs, n.LastExit, replaced.AgentSessionID)
	default:
		return
	}
	h.mu.Lock()
	notify, bosses := h.notify, h.bosses
	h.mu.Unlock()
	to := bosses
	if rec.Parent != "" {
		to = []string{rec.Parent}
	}
	if notify == nil || len(to) == 0 {
		log.Printf("hosting: %s (no recipient)", subject)
		return
	}
	if err := notify(to, subject, body, NoticeKey(n)); err != nil {
		log.Printf("hosting: notice %s: %v", NoticeKey(n), err)
	}
}

func who(r wire.HostedSession) string {
	if r.Label != "" {
		return r.Base + " (label " + r.Label + ")"
	}
	return r.Base
}

func where(r wire.HostedSession) string {
	parts := []string{r.Base}
	if r.Label != "" {
		parts = append(parts, "label "+r.Label)
	}
	parts = append(parts, "node "+r.Node, "directory "+r.Directory)
	return "(" + strings.Join(parts, ", ") + ")"
}
