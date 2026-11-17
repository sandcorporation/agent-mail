package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sandcorporation/agent-mail/internal/wire"
)

// cliProgress reads GET /api/progress (boss roles only; the server answers
// 403 to everyone else): a block per service, or only the named service, by
// default, and the JSON unchanged with --json.
func cliProgress(c Config, p parsedArgs) (int, error) {
	b, err := slotCall(c, "GET", "/api/progress", nil)
	if err != nil {
		return 0, err
	}
	var r wire.ProgressReport
	if err = json.Unmarshal(b, &r); err != nil {
		return 0, fmt.Errorf("unexpected response from server: %v", err)
	}
	if p.bool("--json") {
		fmt.Println(strings.TrimSpace(string(b)))
		return ExitOK, nil
	}
	services := r.Services
	if len(p.pos) == 1 {
		services = nil
		for _, s := range r.Services {
			if s.Service == p.pos[0] {
				services = append(services, s)
			}
		}
		if len(services) == 0 {
			return 0, fmt.Errorf("no service %q in progress", p.pos[0])
		}
	}
	_, err = io.WriteString(os.Stdout, formatProgress(services, r.GeneratedAt))
	return ExitOK, err
}

// formatProgress renders services in the order given (the server sends the
// most recently active first), a blank line between blocks. Relative times
// count from now, the report's generated_at.
func formatProgress(ss []wire.ServiceProgress, now time.Time) string {
	var b strings.Builder
	for i, s := range ss {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s [%s] ", s.Service, s.Badge)
		switch {
		case s.LastReport != nil:
			fmt.Fprintf(&b, "마지막 소식 %s — %s\n", relTime(now, s.LastReport.At), orUntitled(s.LastReport.Subject))
		case s.LastActivityAt != nil:
			fmt.Fprintf(&b, "보고 없음 (마지막 활동 %s)\n", relTime(now, *s.LastActivityAt))
		default:
			b.WriteString("소식 없음\n")
		}
		if len(s.Sessions) > 0 {
			b.WriteString("  세션\n")
			for _, v := range s.Sessions {
				fmt.Fprintf(&b, "    %s %s %s\n", v.Role, v.State, shortDir(v.Directory))
			}
		}
		if len(s.OpenDirectives) > 0 {
			b.WriteString("  답 없는 지시\n")
			for _, d := range s.OpenDirectives {
				mark := " "
				if d.OverThreshold {
					mark = "!"
				}
				fmt.Fprintf(&b, "  %s #%d → %s %s %s 지남 — %s\n", mark, d.ID, d.To, d.Status, humanDur(d.AgeS), orUntitled(d.Subject))
			}
		}
		if len(s.OpenTasks) > 0 {
			b.WriteString("  진행 중 task\n")
			for _, t := range s.OpenTasks {
				fmt.Fprintf(&b, "    %s %s %d통 — %s\n", t.TaskID, relTime(now, t.LastAt), t.Messages, orUntitled(t.LastSubject))
			}
		}
	}
	return b.String()
}

func orUntitled(s string) string {
	if s == "" {
		return "(제목 없음)"
	}
	return s
}

// relTime matches the dashboard's rel(): 방금 under a minute, else
// humanDur + " 전".
func relTime(now, t time.Time) string {
	s := int64(math.Round(now.Sub(t).Seconds()))
	if s < 60 {
		return "방금"
	}
	return humanDur(s) + " 전"
}

// humanDur matches the dashboard's dur(): N초, N분, N시간 and N일, hours and
// days with one decimal below 10.
func humanDur(s int64) string {
	oneDecimal := func(v float64) string {
		if v < 10 {
			return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64)
		}
		return strconv.FormatInt(int64(math.Round(v)), 10)
	}
	switch {
	case s < 60:
		return strconv.FormatInt(max(s, 0), 10) + "초"
	case s < 3600:
		return strconv.FormatInt(int64(math.Round(float64(s)/60)), 10) + "분"
	case s < 86400:
		return oneDecimal(float64(s)/3600) + "시간"
	default:
		return oneDecimal(float64(s)/86400) + "일"
	}
}

// shortDir keeps the last two path elements of a session folder
// (…/workspace/agent-mail); "-" when the session sent none.
func shortDir(dir string) string {
	if dir == "" {
		return "-"
	}
	trimmed := strings.TrimRight(dir, `/\`)
	parts := strings.FieldsFunc(trimmed, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) <= 2 {
		return dir
	}
	return "…/" + strings.Join(parts[len(parts)-2:], "/")
}
