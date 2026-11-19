package hosting

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// RunCLI serves `host add|list|start|stop|remove` against the service's
// database. The client has already checked the action and its arguments;
// add is the record for `host add`, id the record for the others. The host
// picks database changes up when it reconciles, so no server is called.
func (s *Service) RunCLI(action, id string, add wire.HostedSessionCreate, jsonOut bool, stdout io.Writer) error {
	var h wire.HostedSession
	var err error
	switch action {
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
	case "start":
		h, err = s.SetDesired(id, DesiredRunning)
	case "stop":
		h, err = s.SetDesired(id, DesiredStopped)
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
	fmt.Fprintln(tw, "ID\tNODE\tBASE\tLABEL\tDESIRED\tSTATE\tRESTART\tDIRECTORY\tARGS")
	for _, h := range list {
		label := h.Label
		if label == "" {
			label = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", h.ID, h.Node, h.Base, label, h.Desired, h.Runtime.State, h.Restart, h.Directory, strings.Join(h.Args, " "))
	}
	tw.Flush()
}
