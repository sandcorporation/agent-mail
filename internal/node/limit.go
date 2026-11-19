package node

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/term"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// LimitScan is how often the node reads its sessions' screens for the
// agent's usage-limit message (#190).
const LimitScan = 5 * time.Second

// limitPatterns are the agent's usage-limit messages, matched against a
// session's visible screen with its rows joined by spaces (so a wrapped
// message still matches). This is the one list to change when the agent
// words its message differently.
var limitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)usage limit reached`),
	regexp.MustCompile(`(?i)\b(?:5-hour|weekly|session|opus|sonnet) limit reached`),
	regexp.MustCompile(`(?i)you(?:'|’)ve (?:hit|reached) your (?:usage |weekly |session )?limit`),
	regexp.MustCompile(`(?i)limit will reset at`),
}

// resetEpoch is the older form "usage limit reached|<unix seconds>".
var resetEpoch = regexp.MustCompile(`\|(\d{10})\b`)

// resetClock is "resets 3pm", "reset at 10:30pm (Asia/Seoul)", "resets Nov
// 21, 5pm", "resets 21:00".
var resetClock = regexp.MustCompile(`(?i)\bresets?\s+(?:at\s+)?(?:([A-Za-z]{3,9})\s+(\d{1,2}),?\s+(?:at\s+)?)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)?\b(?:\s*\(([A-Za-z_+\-/0-9]+)\))?`)

// LimitHit is a usage-limit message found on a screen: the text from the
// match on (its fingerprint) and the reset time it gave, zero when none.
type LimitHit struct {
	Text  string
	Reset time.Time
}

// DetectLimit looks for the usage-limit message in a screen's rows; at is
// when the screen was read, the reference for a reset time without a date.
func DetectLimit(rows []string, at time.Time) (LimitHit, bool) {
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		if r = strings.TrimSpace(r); r != "" {
			parts = append(parts, r)
		}
	}
	text := strings.Join(parts, " ")
	for _, p := range limitPatterns {
		loc := p.FindStringIndex(text)
		if loc == nil {
			continue
		}
		// The fingerprint ends with the reset clause (or the match), so
		// what the screen shows below the message does not change it.
		window := strings.ToValidUTF8(text[loc[0]:min(len(text), loc[1]+120)], "")
		fp := text[loc[0]:loc[1]]
		for _, re := range []*regexp.Regexp{resetEpoch, resetClock} {
			if r := re.FindStringIndex(window); r != nil {
				fp = window[:r[1]]
				break
			}
		}
		return LimitHit{Text: fp, Reset: ParseReset(fp, at)}, true
	}
	return LimitHit{}, false
}

// ParseReset reads the reset time out of a usage-limit message seen at at:
// the next such clock time after at (in the zone the message names, else
// at's zone), or the unix time of the older "|<seconds>" form. It returns
// zero when the message gives none.
func ParseReset(msg string, at time.Time) time.Time {
	if m := resetEpoch.FindStringSubmatch(msg); m != nil {
		sec, _ := strconv.ParseInt(m[1], 10, 64)
		return time.Unix(sec, 0)
	}
	m := resetClock.FindStringSubmatch(msg)
	if m == nil {
		return time.Time{}
	}
	hour, _ := strconv.Atoi(m[3])
	minute := 0
	if m[4] != "" {
		minute, _ = strconv.Atoi(m[4])
	}
	if m[5] != "" && (hour < 1 || hour > 12) {
		return time.Time{}
	}
	switch strings.ToLower(m[5]) {
	case "am":
		if hour == 12 {
			hour = 0
		}
	case "pm":
		if hour < 12 {
			hour += 12
		}
	}
	if hour > 23 || minute > 59 {
		return time.Time{}
	}
	loc := at.Location()
	if m[6] != "" {
		if l, err := time.LoadLocation(m[6]); err == nil {
			loc = l
		}
	}
	ref := at.In(loc)
	if m[1] != "" {
		month, ok := monthOf(m[1])
		day, _ := strconv.Atoi(m[2])
		if !ok || day < 1 || day > 31 {
			return time.Time{}
		}
		t := time.Date(ref.Year(), month, day, hour, minute, 0, 0, loc)
		if !t.After(at) {
			t = t.AddDate(1, 0, 0)
		}
		return t
	}
	t := time.Date(ref.Year(), ref.Month(), ref.Day(), hour, minute, 0, 0, loc)
	if !t.After(at) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

func monthOf(s string) (time.Month, bool) {
	s = strings.ToLower(s)
	if len(s) < 3 {
		return 0, false
	}
	for m := time.January; m <= time.December; m++ {
		if strings.HasPrefix(strings.ToLower(m.String()), s[:3]) {
			return m, true
		}
	}
	return 0, false
}

// limitSeen is one session's showing of the message: its fingerprint, the
// reset it gave, when the node first saw it, and whether it is spent (its
// reset passed). A spent showing stays ignored until the screen changes.
type limitSeen struct {
	text  string
	reset time.Time
	since time.Time
	spent bool
}

// scanLimits reads the screens of the live assigned sessions, at most every
// LimitScan, and updates what each shows. A session no longer showing the
// message drops its entry; one whose reset time passed is spent. Read
// errors leave the entry as it was. n.mu is held.
func (n *Node) scanLimits(ctx context.Context, byName map[string]term.Info) {
	now := n.cfg.Now()
	if !n.lastScan.IsZero() && now.Sub(n.lastScan) < LimitScan {
		n.expireLimits(now)
		return
	}
	n.lastScan = now
	live := map[string]bool{}
	for _, r := range n.records {
		in, ok := byName[TerminalName(r.ID)]
		if !ok || !in.Alive {
			continue
		}
		live[r.ID] = true
		g, err := n.backend.Screen(ctx, in.Name, 0)
		if err != nil {
			continue
		}
		hit, found := DetectLimit(g.ScreenText(), now)
		cur := n.limits[r.ID]
		switch {
		case !found:
			delete(n.limits, r.ID)
		case cur == nil || cur.text != hit.Text:
			n.limits[r.ID] = &limitSeen{text: hit.Text, reset: hit.Reset, since: now}
		}
	}
	for id := range n.limits {
		if !live[id] {
			delete(n.limits, id)
		}
	}
	n.expireLimits(now)
}

func (n *Node) expireLimits(now time.Time) {
	for _, l := range n.limits {
		if !l.reset.IsZero() && !now.Before(l.reset) {
			l.spent = true
		}
	}
}

// Limit reports the usage limit as the node's screens show it: limited
// while any session shows a message that is not spent.
func (n *Node) Limit() wire.NodeLimit {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out wire.NodeLimit
	var reset int64
	for _, l := range n.limits {
		if l.spent {
			continue
		}
		out.Limited = true
		out.Since = max(out.Since, l.since.UnixMilli())
		if !l.reset.IsZero() {
			reset = max(reset, l.reset.UnixMilli())
		}
	}
	if out.Limited && reset > 0 {
		out.ResetAt = &reset
	}
	return out
}
