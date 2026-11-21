package hosting

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// RunAutoCLI serves `host auto add|list|show|run|pause|resume|rm` against
// the service's database. ref is the hosted session for add and the
// scheduled run's id for the others; runs is show's --runs. The host picks
// the changes up on its next sweep, so no server is called.
func (s *Service) RunAutoCLI(action, ref string, c wire.HostAutoCreate, runs int, jsonOut bool, stdout io.Writer) error {
	switch action {
	case "add":
		a, err := s.CreateAuto(ref, c)
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("no hosted session %s", ref)
		}
		if err != nil {
			return err
		}
		if jsonOut {
			return json.NewEncoder(stdout).Encode(a)
		}
		fmt.Fprintln(stdout, a.ID)
		return nil
	case "list":
		list, err := s.Autos("")
		if err != nil {
			return err
		}
		if jsonOut {
			return json.NewEncoder(stdout).Encode(list)
		}
		printAutos(stdout, list)
		return nil
	}
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("no scheduled run %s", ref)
	}
	if action == "show" {
		v, err := s.AutoView(id, runs)
		if err != nil {
			return autoCLIError(err, ref)
		}
		if jsonOut {
			return json.NewEncoder(stdout).Encode(v)
		}
		printAutoView(stdout, v)
		return nil
	}
	var a wire.HostAuto
	switch action {
	case "run":
		a, err = s.RequestAutoRun(id)
	case "pause":
		a, err = s.PauseAuto(id)
	case "resume":
		a, err = s.ResumeAuto(id)
	case "rm":
		a, err = s.DeleteAuto(id)
	default:
		return fmt.Errorf("host auto: unknown action %q", action)
	}
	if err != nil {
		return autoCLIError(err, ref)
	}
	if jsonOut {
		return json.NewEncoder(stdout).Encode(a)
	}
	switch action {
	case "run":
		fmt.Fprintf(stdout, "%d run requested (the host runs it on its next sweep)\n", a.ID)
	case "rm":
		fmt.Fprintf(stdout, "removed %d\n", a.ID)
	default:
		fmt.Fprintf(stdout, "%d %s, next %s\n", a.ID, a.State, msTime(a.NextFireAt))
	}
	return nil
}

func autoCLIError(err error, ref string) error {
	if errors.Is(err, ErrNoSuchAuto) {
		return fmt.Errorf("no scheduled run %s", ref)
	}
	return err
}

func msTime(ms *int64) string {
	if ms == nil {
		return "-"
	}
	return time.UnixMilli(*ms).Local().Format("2006-01-02 15:04")
}

func lastRun(r *wire.HostAutoRun) string {
	if r == nil {
		return "-"
	}
	return r.Result + " " + msTime(&r.At)
}

func printAutos(w io.Writer, list []wire.HostAuto) {
	if len(list) == 0 {
		fmt.Fprintln(w, "no scheduled runs")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSESSION\tCRON\tTZ\tSTATE\tNEXT\tLAST\tLABEL")
	for _, a := range list {
		label := a.Label
		if label == "" {
			label = "-"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.Session, a.Cron, a.TZ, a.State, msTime(a.NextFireAt), lastRun(a.LastRun), label)
	}
	tw.Flush()
}

func printAutoView(w io.Writer, v wire.HostAutoView) {
	mode := "new conversation"
	if v.Resume {
		mode = "resume"
	}
	fmt.Fprintf(w, "scheduled run %d of %s\n", v.ID, v.Session)
	fmt.Fprintf(w, "cron %s (%s), %s, next %s\n", v.Cron, v.TZ, v.State, msTime(v.NextFireAt))
	fmt.Fprintf(w, "launch: %s, max-run %s\n", mode, time.Duration(v.MaxRunMS)*time.Millisecond)
	if v.Label != "" {
		fmt.Fprintf(w, "label: %s\n", v.Label)
	}
	if len(v.Notify) > 0 {
		fmt.Fprintf(w, "notify: %v\n", v.Notify)
	}
	fmt.Fprintf(w, "prompt:\n%s\n", v.Prompt)
	if len(v.Runs) == 0 {
		fmt.Fprintln(w, "no runs yet")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AT\tRESULT\tMAIL\tLAUNCHED\tDETAIL")
	for _, r := range v.Runs {
		mailID, launched, detail := "-", "-", r.Detail
		if r.MailID != nil {
			mailID = strconv.FormatInt(*r.MailID, 10)
		}
		if r.Launched {
			launched = r.AgentSessionID
		}
		if r.Manual {
			detail = "(run now) " + detail
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", msTime(&r.At), r.Result, mailID, launched, detail)
	}
	tw.Flush()
}
