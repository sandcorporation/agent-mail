package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/sandcorporation/agent-mail/internal/wire"
)

// decisionQueryOptions maps CLI options to GET /api/decisions parameters.
var decisionQueryOptions = []struct{ flag, param string }{
	{"--from", "from"}, {"--to", "to"}, {"--role", "role"}, {"--q", "q"},
}

// cliDecisions reads GET /api/decisions: a compact block per decision by
// default, the JSON unchanged with --json, the ndjson export streamed
// unchanged with --ndjson.
func cliDecisions(c Config, p parsedArgs) (int, error) {
	ndjson := p.bool("--ndjson")
	if ndjson && p.bool("--json") {
		return 0, usagef("use either --json or --ndjson, not both")
	}
	q := url.Values{}
	for _, o := range decisionQueryOptions {
		if v, ok := p.values[o.flag]; ok && v != "" {
			q.Set(o.param, v)
		}
	}
	if ndjson {
		q.Set("format", "ndjson")
	}
	path := "/api/decisions"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	if ndjson {
		return ExitOK, streamCall(c, path, os.Stdout)
	}
	b, err := slotCall(c, "GET", path, nil)
	if err != nil {
		return 0, err
	}
	var list wire.DecisionList
	if err = json.Unmarshal(b, &list); err != nil {
		return 0, fmt.Errorf("unexpected response from server: %v", err)
	}
	if p.bool("--json") {
		fmt.Println(strings.TrimSpace(string(b)))
		return ExitOK, nil
	}
	_, err = io.WriteString(os.Stdout, formatDecisions(list.Decisions))
	return ExitOK, err
}

// formatDecisions renders decisions in the order given (the server sends
// newest first), a blank line between blocks.
func formatDecisions(ds []wire.Decision) string {
	var b strings.Builder
	for i, d := range ds {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s %s %s (by %s)\n", d.ID, d.AnsweredAt.Local().Format("2006-01-02 15:04"), d.Asker, d.AnsweredBy)
		for _, it := range d.Items {
			answer := strings.Join(it.Chosen, ", ")
			if it.Other != "" {
				if answer != "" {
					answer += " / "
				}
				answer += "기타: " + it.Other
			}
			fmt.Fprintf(&b, "  %s: %s\n", it.Header, answer)
		}
	}
	return b.String()
}

// streamCall copies a GET response body to w as it arrives; a non-2xx status
// is an error like slotCall's.
func streamCall(c Config, path string, w io.Writer) error {
	req, err := http.NewRequest("GET", endpoint(c, path), nil)
	if err != nil {
		return err
	}
	authorize(c, req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	_, err = io.Copy(w, resp.Body)
	return err
}
