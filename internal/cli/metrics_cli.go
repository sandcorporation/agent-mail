package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sandcorporation/agent-mail/internal/wire"
)

// cliMetrics prints GET /api/metrics as a compact summary.
func cliMetrics(c Config, p parsedArgs) (int, error) {
	path := "/api/metrics"
	if v, ok := p.values["--days"]; ok {
		if _, err := wire.ParseMetricsDays(v); err != nil || v == "" {
			return 0, usagef("--days must be a whole number from 1 to %d", wire.MaxMetricsDays)
		}
		path += "?days=" + v
	}
	b, err := slotCall(c, "GET", path, nil)
	if err != nil {
		return 0, err
	}
	var m wire.MetricsReport
	if err = json.Unmarshal(b, &m); err != nil {
		return 0, fmt.Errorf("unexpected response from server: %v", err)
	}
	if p.bool("--json") {
		fmt.Println(strings.TrimSpace(string(b)))
		return ExitOK, nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "window\t%s - %s (%d days)\n", m.Window.From.Local().Format("2006-01-02 15:04"), m.Window.To.Local().Format("2006-01-02 15:04"), m.Window.Days)
	fmt.Fprintf(tw, "first reply\t%s\n", latencyLine(m.FirstReply.LatencyStats))
	for _, r := range m.FirstReply.ByRecipient {
		fmt.Fprintf(tw, "  %s\t%s\n", r.Base, latencyLine(r.LatencyStats))
	}
	fmt.Fprintf(tw, "pending\t%s\n", plural(len(m.Pending), "role"))
	for _, r := range m.Pending {
		mark := ""
		if r.OverThreshold {
			mark = "  over " + compactSeconds(m.Thresholds.PendingS)
		}
		fmt.Fprintf(tw, "  %s\t%d, oldest %s%s\n", r.Role, r.Pending, compactSeconds(r.OldestS), mark)
	}
	fmt.Fprintf(tw, "threads\t%s with 2+ round trips\n", plural(m.Threads.MultiRound, "thread"))
	for _, t := range m.Threads.Top {
		fmt.Fprintf(tw, "  #%d\t%d round trips, %s: %s\n", t.ThreadID, t.RoundTrips, strings.Join(t.Parties, " <-> "), t.Subject)
	}
	fmt.Fprintf(tw, "sessions\t%s with error or blocked time\n", plural(len(m.Sessions.ByRole), "role"))
	for _, r := range m.Sessions.ByRole {
		kinds := make([]string, 0, len(r.TopKinds))
		for _, k := range r.TopKinds {
			kinds = append(kinds, k.State+" "+compactSeconds(k.Seconds))
		}
		fmt.Fprintf(tw, "  %s\terror %s, blocked %s (%s)\n", r.Role, compactSeconds(r.ErrorS), compactSeconds(r.BlockedS), strings.Join(kinds, ", "))
	}
	for _, c := range m.Sessions.Current {
		fmt.Fprintf(tw, "  now %s\t%s for %s (since %s)\n", c.Role, c.State, compactSeconds(c.Seconds), c.Since.Local().Format("2006-01-02 15:04"))
	}
	return ExitOK, tw.Flush()
}

func latencyLine(s wire.LatencyStats) string {
	line := fmt.Sprintf("%d sent, %d answered, %d unanswered", s.Count, s.Answered, s.Unanswered)
	if s.P50 != nil {
		line += fmt.Sprintf("; p50 %s, p90 %s, max %s", compactSeconds(*s.P50), compactSeconds(*s.P90), compactSeconds(*s.Max))
	}
	return line
}

// compactSeconds shows a duration in whole seconds compactly: 0s, 45s, 12m, 2h5m, 3d4h.
func compactSeconds(n int64) string {
	d := time.Duration(n) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", n)
	case d < time.Hour:
		return fmt.Sprintf("%dm", n/60)
	case d < 24*time.Hour:
		if m := n / 60 % 60; m > 0 {
			return fmt.Sprintf("%dh%dm", n/3600, m)
		}
		return fmt.Sprintf("%dh", n/3600)
	default:
		if h := n / 3600 % 24; h > 0 {
			return fmt.Sprintf("%dd%dh", n/86400, h)
		}
		return fmt.Sprintf("%dd", n/86400)
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
