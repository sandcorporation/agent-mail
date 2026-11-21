package hosting

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// AutoMail puts one system mail (from switchboard/system) into role's
// mailbox, idempotent by key, and returns its id; a second call with the
// same key returns the first mail's id. The server passes it in; hosting
// does not import mail.
type AutoMail func(role, subject, body, key string) (int64, error)

// SetAutoMail installs the mail function of scheduled runs. Without it a
// fired run records an error.
func (h *Hub) SetAutoMail(fn AutoMail) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.autoMail = fn
}

// AutoMailKey is the idempotency key of a scheduled run's mail:
// hostauto:<id>:<time> for a scheduled time, hostauto:<id>:run:<time> for a
// `host auto run`.
func AutoMailKey(id int64, at int64, manual bool) string {
	if manual {
		return "hostauto:" + strconv.FormatInt(id, 10) + ":run:" + strconv.FormatInt(at, 10)
	}
	return "hostauto:" + strconv.FormatInt(id, 10) + ":" + strconv.FormatInt(at, 10)
}

// SweepAutos fires every scheduled run whose time has come or that was
// asked to run, once each: a stretch of missed times (the host was down)
// fires once. It returns how many it fired or recorded.
func (h *Hub) SweepAutos(ctx context.Context) (int, error) {
	now := h.svc.clock()
	due, err := h.svc.dueAutos(now)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range due {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		fire, manual, at, err := h.svc.claimAuto(a, now)
		if err != nil {
			return n, err
		}
		if !fire {
			continue
		}
		if _, err := h.fireAuto(a, at, manual); err != nil {
			log.Printf("hosting: scheduled run %d: %v", a.ID, err)
		}
		n++
	}
	return n, nil
}

// RunAuto runs the scheduled run id once now, whatever its time and state
// (the dashboard's "run now").
func (h *Hub) RunAuto(id int64) (wire.HostAutoRun, error) {
	a, err := h.svc.GetAuto(id)
	if err != nil {
		return wire.HostAutoRun{}, err
	}
	return h.fireAuto(a, h.svc.clock().UnixMilli(), true)
}

// fireAuto runs a once and records the result. The session's node must be
// attached to this host; the session is launched (a new conversation, or
// the last one with Resume) only when it is set to stop, so a session
// already running only gets the mail.
func (h *Hub) fireAuto(a wire.HostAuto, at int64, manual bool) (wire.HostAutoRun, error) {
	run := wire.HostAutoRun{Auto: a.ID, At: h.svc.clock().UnixMilli(), Manual: manual}
	finish := func(result, detail string) (wire.HostAutoRun, error) {
		run.Result, run.Detail = result, detail
		out, err := h.svc.addAutoRun(run)
		h.publish("terminals")
		return out, err
	}
	rec, err := h.svc.Get(a.Session)
	if err != nil {
		return finish(wire.AutoError, "hosted session: "+err.Error())
	}
	h.mu.Lock()
	_, attached := h.conns[rec.Node]
	send := h.autoMail
	h.mu.Unlock()
	if !attached {
		return finish(wire.AutoNodeOffline, "node "+rec.Node+" is not attached")
	}
	// The precheck (#334) and the overlap rule (#335) go here.
	if rec.Desired != DesiredRunning {
		var launched wire.HostedSession
		if a.Resume {
			launched, err = h.svc.SetDesired(rec.ID, DesiredRunning)
		} else {
			launched, err = h.svc.NewConversation(rec.ID)
		}
		var capErr CapacityError
		switch {
		case errors.As(err, &capErr):
			return finish(wire.AutoWaiting, capErr.Error())
		case err != nil:
			return finish(wire.AutoError, "launch: "+err.Error())
		}
		run.Launched, run.AgentSessionID = true, launched.AgentSessionID
		if err := h.Sync(); err != nil {
			log.Printf("hosting: sync: %v", err)
		}
	}
	if send == nil {
		return finish(wire.AutoError, "no mail function")
	}
	id, err := send(rec.Base, autoSubject(a), a.Prompt, AutoMailKey(a.ID, at, manual))
	if err != nil {
		return finish(wire.AutoError, "mail: "+err.Error())
	}
	run.MailID = &id
	return finish(wire.AutoFired, "")
}

// autoSubject is the mail subject of a scheduled run: its label, else
// "scheduled run <id>".
func autoSubject(a wire.HostAuto) string {
	if a.Label != "" {
		return a.Label
	}
	return fmt.Sprintf("scheduled run %d", a.ID)
}
