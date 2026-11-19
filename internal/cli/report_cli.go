package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// cliReport posts a report (POST /api/reports, boss roles only) and prints
// its ID, or the stored report with --json.
func cliReport(c Config, p parsedArgs, out io.Writer) (int, error) {
	q, err := reportRequest(p)
	if err != nil {
		return 0, err
	}
	b, err := slotCall(c, "POST", "/api/reports", q)
	if err != nil {
		return 0, err
	}
	if p.bool("--json") {
		_, err = fmt.Fprintln(out, strings.TrimSpace(string(b)))
		return ExitOK, err
	}
	var rp wire.Report
	if err = json.Unmarshal(b, &rp); err != nil {
		return 0, fmt.Errorf("unexpected response from server: %v", err)
	}
	_, err = fmt.Fprintln(out, rp.ID)
	return ExitOK, err
}

// reportRequest builds the request from the options: --title is required,
// --mail may repeat and each value may list IDs separated by commas.
func reportRequest(p parsedArgs) (wire.ReportRequest, error) {
	q := wire.ReportRequest{Title: p.values["--title"], Request: p.values["--request"], IdempotencyKey: p.values["--idem"]}
	if strings.TrimSpace(q.Title) == "" {
		return q, usagef("report needs --title")
	}
	for _, v := range p.multi["--mail"] {
		for _, f := range strings.Split(v, ",") {
			f = strings.TrimPrefix(strings.TrimSpace(f), "#")
			if f == "" {
				continue
			}
			id, err := strconv.ParseInt(f, 10, 64)
			if err != nil || id <= 0 {
				return q, usagef("--mail takes message IDs, got %q", f)
			}
			q.Mail = append(q.Mail, id)
		}
	}
	body, err := inputBody(p)
	if err != nil {
		return q, err
	}
	q.Body = body
	return q, nil
}
