package hosting

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// checkGrace is how long the host waits for a precheck's answer beyond its
// limit: the node's kill, the reply and the socket.
const checkGrace = 30 * time.Second

// pendingCheck is a precheck frame waiting for its answer from conn.
type pendingCheck struct {
	conn *nodeConn
	ch   chan wire.NodeCheckResult
}

// precheck runs a's precheck on the node of rec (#334) and fills run's exit
// code, duration and output. result is "" when the run goes on (exit 0),
// else the run's result with detail.
func (h *Hub) precheck(a wire.HostAuto, rec wire.HostedSession, run *wire.HostAutoRun) (result, detail string) {
	timeout := a.CheckTimeoutMS
	if timeout <= 0 {
		timeout = wire.DefaultCheckTimeoutMS
	}
	res, err := h.askCheck(rec.Node, rec.ID, wire.NodeCheck{Command: a.Check, TimeoutMS: timeout}, time.Duration(timeout)*time.Millisecond+checkGrace)
	if err != nil {
		return wire.AutoError, "precheck: " + err.Error()
	}
	dur := res.DurationMS
	run.DurationMS, run.Output, run.ExitCode = &dur, res.Output, res.ExitCode
	switch {
	case res.Error != "":
		return wire.AutoError, "precheck: " + res.Error
	case res.TimedOut:
		return wire.AutoTimeout, fmt.Sprintf("precheck: no exit within %s; killed", time.Duration(timeout)*time.Millisecond)
	case res.ExitCode == nil:
		return wire.AutoError, "precheck: no exit code"
	case *res.ExitCode == 0:
		return "", ""
	case *res.ExitCode == 1:
		return wire.AutoSkipped, "precheck: exit 1"
	}
	return wire.AutoError, "precheck: exit " + strconv.Itoa(*res.ExitCode)
}

// askCheck sends a check frame for record id to node and waits up to wait
// for its answer; the node leaving ends the wait.
func (h *Hub) askCheck(node, id string, chk wire.NodeCheck, wait time.Duration) (wire.NodeCheckResult, error) {
	h.mu.Lock()
	c, ok := h.conns[node]
	if !ok {
		h.mu.Unlock()
		return wire.NodeCheckResult{}, fmt.Errorf("node %s is not attached", node)
	}
	if h.prechecks == nil {
		h.prechecks = map[string]pendingCheck{}
	}
	h.checkSeq++
	chk.Ref = "c" + strconv.FormatInt(h.checkSeq, 10)
	ch := make(chan wire.NodeCheckResult, 1)
	h.prechecks[chk.Ref] = pendingCheck{conn: c, ch: ch}
	sent := h.sendLocked(c, wire.NodeFrame{Type: wire.NodeCheckType, ID: id, Check: &chk})
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.prechecks, chk.Ref)
		h.mu.Unlock()
	}()
	if !sent {
		return wire.NodeCheckResult{}, fmt.Errorf("node %s: send failed", node)
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case res := <-ch:
		return res, nil
	case <-t.C:
		return wire.NodeCheckResult{}, fmt.Errorf("node %s did not answer within %s (a node older than the precheck does not)", node, wait)
	}
}

// checkAnswered hands a check-result frame from c to its waiter.
func (h *Hub) checkAnswered(c *nodeConn, res wire.NodeCheckResult) {
	h.mu.Lock()
	p, ok := h.prechecks[res.Ref]
	if ok && p.conn == c {
		delete(h.prechecks, res.Ref)
	}
	h.mu.Unlock()
	if ok && p.conn == c {
		p.ch <- res
	}
}

// dropChecksLocked ends the waits on c, which is gone.
func (h *Hub) dropChecksLocked(c *nodeConn) {
	for ref, p := range h.prechecks {
		if p.conn == c {
			delete(h.prechecks, ref)
			p.ch <- wire.NodeCheckResult{Ref: ref, Error: "node " + c.name + " left before the precheck ended"}
		}
	}
}

// withCheckOutput is a fired run's mail body: the prompt, then the
// precheck's output when it printed any.
func withCheckOutput(prompt, output string) string {
	if strings.TrimSpace(output) == "" {
		return prompt
	}
	return prompt + "\n\n" + strings.TrimRight(output, "\r\n")
}
