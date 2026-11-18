package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// cliStatus prints GET /api/presence as a table.
func cliStatus(c Config, p parsedArgs) (int, error) {
	path := "/api/presence"
	if len(p.pos) == 1 {
		path += "?role=" + url.QueryEscape(p.pos[0])
	}
	b, err := slotCall(c, "GET", path, nil)
	if err != nil {
		return 0, err
	}
	var out wire.PresenceList
	if err = json.Unmarshal(b, &out); err != nil {
		return 0, fmt.Errorf("unexpected response from server: %v", err)
	}
	if p.bool("--json") {
		fmt.Println(strings.TrimSpace(string(b)))
		return ExitOK, nil
	}
	if len(out.Sessions) == 0 {
		fmt.Println("no sessions")
		return ExitOK, nil
	}
	return ExitOK, writeStatusTable(os.Stdout, out)
}

// writeStatusTable prints the sessions as the status table. New columns go
// at the end: scripts may read the table by position.
func writeStatusTable(w io.Writer, out wire.PresenceList) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tSTATUS\tCONTEXT\tLAST TURN\tMACHINE\tDIRECTORY\tLABEL\tPARENT\tREPORTED\tSINCE")
	for _, v := range out.Sessions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", v.Role, v.Status, kTokens(v.ContextTokens),
			clockTime(v.LastTurnAt), v.Machine, v.Directory, dash(v.Label), dash(v.Parent), ago(out.Now, v.ReportedAt), elapsed(out.Now, v.Since))
	}
	return tw.Flush()
}

// elapsed is the time since t on the server's clock, such as 7h12m.
func elapsed(now time.Time, t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return compactSeconds(max(0, int64(now.Sub(*t)/time.Second)))
}

// kTokens shows a token count in thousands: 950 -> 0.9k, 12345 -> 12k.
func kTokens(n *int64) string {
	switch {
	case n == nil:
		return "-"
	case *n < 10000:
		return strconv.FormatFloat(float64(*n)/1000, 'f', 1, 64) + "k"
	default:
		return strconv.FormatInt((*n+500)/1000, 10) + "k"
	}
}

func clockTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// ago is the time since the last report, measured on the server's clock.
func ago(now time.Time, t *time.Time) string {
	if t == nil {
		return "-"
	}
	d := now.Sub(*t).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String() + " ago"
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
