package hosting

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

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

// AutoMailDone reports whether the mail id a scheduled run sent was
// completed by its recipient. The server passes it in (mail knows); hosting
// does not import mail.
type AutoMailDone func(id int64) (bool, error)

// SetAutoMailDone installs the completion check of scheduled runs' mail.
// Without it a session a run launched stops only at its max-run.
func (h *Hub) SetAutoMailDone(fn AutoMailDone) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.autoMailDone = fn
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

// SweepAutos first stops the sessions scheduled runs launched whose run is
// over (StopAutoSessions), then fires every scheduled run whose time has
// come or that was asked to run, once each: a stretch of missed times (the
// host was down) fires once. It returns how many it fired or recorded.
func (h *Hub) SweepAutos(ctx context.Context) (int, error) {
	if _, err := h.StopAutoSessions(); err != nil {
		return 0, err
	}
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
	// Overlap (#335): a session an earlier run launched still runs for that
	// run, so this one is busy, before any precheck (#334).
	if held, err := h.heldBy(rec); err != nil {
		return finish(wire.AutoError, "overlap: "+err.Error())
	} else if held != nil {
		return finish(wire.AutoBusy, fmt.Sprintf("run %d of scheduled run %d still holds the session", held.ID, held.Auto))
	}
	// The precheck (#334) goes here.
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
		launchedAt := h.svc.clock().UnixMilli()
		run.Launched, run.AgentSessionID, run.LaunchedAt = true, launched.AgentSessionID, &launchedAt
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

// heldBy is the run that launched rec and still holds it, or nil. A held run
// whose session no longer runs is ended first (it was stopped by hand).
func (h *Hub) heldBy(rec wire.HostedSession) (*wire.HostAutoRun, error) {
	held, err := h.svc.heldAutoRuns(rec.ID)
	if err != nil || len(held) == 0 {
		return nil, err
	}
	if rec.Desired != DesiredRunning {
		now := h.svc.clock().UnixMilli()
		for _, r := range held {
			if _, err := h.svc.endAutoRun(r.ID, now, "", "session stopped"); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	return &held[0], nil
}

// StopAutoSessions ends every run that launched a session the host still
// watches and whose run is over: the session is stopped (desired stopped)
// when the run's mail was completed, or when max-run passed since the launch
// (the run's result becomes overrun). A session that was stopped or removed
// otherwise is only no longer watched. Sessions a run did not launch (they
// already ran) are never stopped. It returns how many runs it ended.
func (h *Hub) StopAutoSessions() (int, error) {
	held, err := h.svc.heldAutoRuns("")
	if err != nil || len(held) == 0 {
		return 0, err
	}
	h.mu.Lock()
	done := h.autoMailDone
	h.mu.Unlock()
	now := h.svc.clock().UnixMilli()
	n, stopped := 0, false
	for _, r := range held {
		result, detail, stop, err := h.autoRunOver(r, now, done)
		if err != nil {
			log.Printf("hosting: scheduled run %d, run %d: %v", r.Auto, r.ID, err)
			continue
		}
		if detail == "" {
			continue
		}
		if stop {
			a, _ := h.svc.GetAuto(r.Auto)
			if _, err := h.svc.SetDesired(a.Session, DesiredStopped); err != nil && !errors.Is(err, ErrNotFound) {
				log.Printf("hosting: scheduled run %d: stop %s: %v", r.Auto, a.Session, err)
				continue
			}
			stopped = true
		}
		ended, err := h.svc.endAutoRun(r.ID, now, result, detail)
		if err != nil {
			return n, err
		}
		if ended {
			n++
		}
	}
	if stopped {
		if err := h.Sync(); err != nil {
			log.Printf("hosting: sync: %v", err)
		}
	}
	if n > 0 {
		h.publish("terminals")
	}
	return n, nil
}

// autoRunOver decides whether the held run r is over at now: detail is
// empty while it goes on; stop says to stop its session; result is
// overrun when max-run passed, else empty (the run keeps its result).
func (h *Hub) autoRunOver(r wire.HostAutoRun, now int64, done AutoMailDone) (result, detail string, stop bool, err error) {
	a, err := h.svc.GetAuto(r.Auto)
	if errors.Is(err, ErrNoSuchAuto) {
		return "", "scheduled run removed", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	rec, err := h.svc.Get(a.Session)
	if errors.Is(err, ErrNotFound) {
		return "", "session removed", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	if rec.Desired != DesiredRunning {
		return "", "session stopped", false, nil
	}
	if r.MailID != nil && done != nil {
		ok, err := done(*r.MailID)
		if err != nil {
			return "", "", false, err
		}
		if ok {
			return "", "mail completed, session stopped", true, nil
		}
	}
	start := r.At
	if r.LaunchedAt != nil {
		start = *r.LaunchedAt
	}
	maxRun := a.MaxRunMS
	if maxRun <= 0 {
		maxRun = wire.DefaultAutoMaxRunMS
	}
	if now-start >= maxRun {
		return wire.AutoOverrun, "max-run " + time.Duration(maxRun*int64(time.Millisecond)).String() + " passed, session stopped", true, nil
	}
	return "", "", false, nil
}
