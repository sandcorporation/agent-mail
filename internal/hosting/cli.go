package hosting

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// RunCLI serves `host add|list|set|start|stop|restart|new-conversation|remove|limit` against the service's
// database. The client has already checked the action and its arguments;
// add is the record for `host add`, id the record for the others. The host
// picks database changes up when it reconciles, so no server is called.
func (s *Service) RunCLI(action, id string, add wire.HostedSessionCreate, jsonOut bool, stdout io.Writer) error {
	var h wire.HostedSession
	var err error
	switch action {
	case "limit":
		l := wire.HostingLimit{}
		var err error
		switch id {
		case "on", "off":
			l, err = s.SetLimit(id == "on")
		default:
			l, err = s.Limit()
		}
		if err != nil {
			return err
		}
		if jsonOut {
			return json.NewEncoder(stdout).Encode(l)
		}
		printLimit(stdout, l)
		return nil
	case "list":
		list, err := s.List()
		if err != nil {
			return err
		}
		if jsonOut {
			return json.NewEncoder(stdout).Encode(list)
		}
		printList(stdout, list)
		return nil
	case "add":
		h, err = s.Create(add)
	case "set":
		h, err = s.Update(id, wire.HostedSessionChange{SkipPermissions: add.SkipPermissions})
	case "start":
		h, err = s.SetDesired(id, DesiredRunning)
	case "stop":
		h, err = s.SetDesired(id, DesiredStopped)
	case "restart":
		h, err = s.Restart(id)
	case "new-conversation":
		h, err = s.NewConversation(id)
	case "remove":
		h, err = s.Delete(id)
	default:
		return fmt.Errorf("host: unknown action %q", action)
	}
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("no hosted session %s", id)
		}
		return err
	}
	if jsonOut {
		return json.NewEncoder(stdout).Encode(h)
	}
	switch action {
	case "add":
		fmt.Fprintln(stdout, h.ID)
	case "remove":
		fmt.Fprintf(stdout, "removed %s\n", h.ID)
	case "set":
		fmt.Fprintf(stdout, "%s permissions %s (from the next launch or restart)\n", h.ID, Perms(h))
	case "restart":
		fmt.Fprintf(stdout, "%s restart requested\n", h.ID)
	case "new-conversation":
		fmt.Fprintf(stdout, "%s new conversation %s\n", h.ID, h.AgentSessionID)
	default:
		fmt.Fprintf(stdout, "%s %s\n", h.ID, h.Desired)
	}
	return nil
}

func printList(w io.Writer, list []wire.HostedSession) {
	if len(list) == 0 {
		fmt.Fprintln(w, "no hosted sessions")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNODE\tBASE\tCLASS\tPERMS\tLABEL\tDESIRED\tSTATE\tWORK\tRESTART\tDIRECTORY\tARGS")
	for _, h := range list {
		label := h.Label
		if label == "" {
			label = "-"
		}
		work := "yes"
		if !h.AcceptingWork {
			work = "no"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", h.ID, h.Node, h.Base, h.Class, Perms(h), label, h.Desired, h.Runtime.State, work, h.Restart, h.Directory, strings.Join(h.Args, " "))
	}
	tw.Flush()
}

// Perms is a record's permission mode as host list shows it: "skip" when
// the agent launches with its permission-skip arguments, "ask" otherwise.
func Perms(h wire.HostedSession) string {
	if h.SkipPermissions {
		return "skip"
	}
	return "ask"
}

func printLimit(w io.Writer, l wire.HostingLimit) {
	if !l.Limited {
		fmt.Fprintln(w, "not limited")
		return
	}
	line := "limited (" + l.Source + ")"
	if l.Since != nil {
		line += " since " + time.UnixMilli(*l.Since).Format(time.RFC3339)
	}
	if l.ResetAt != nil {
		line += ", resets " + time.UnixMilli(*l.ResetAt).Format(time.RFC3339)
	}
	fmt.Fprintln(w, line)
}
