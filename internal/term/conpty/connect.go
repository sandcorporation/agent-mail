package conpty

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ConnectOptions says how the node reaches its keeper.
type ConnectOptions struct {
	Dial Dialer
	// Version is the node's switchboard version; a keeper reporting another
	// one is replaced.
	Version string
	// Start starts a keeper process that outlives the caller.
	Start func() error
	// Grace is what a replaced keeper gives its children (<= 0:
	// DefaultGrace).
	Grace time.Duration
	// Wait bounds waiting for a started keeper to answer and for a replaced
	// one to go (<= 0: 15 s).
	Wait time.Duration
}

// Connect returns a client of the running keeper, starting one when none
// answers. A keeper of the node's version is adopted with its sessions; one
// of another version (or protocol) is shut down, its children stopped, and a
// new keeper started. The node then finds no sessions and launches every
// record with --resume, which is the relaunch path.
func Connect(ctx context.Context, o ConnectOptions) (*Client, Adoption, error) {
	if o.Wait <= 0 {
		o.Wait = 15 * time.Second
	}
	if o.Grace <= 0 {
		o.Grace = DefaultGrace
	}
	c := NewClient(o.Dial)
	ki, err := c.Version(ctx)
	v, proto := ki.Version, ki.Protocol
	switch {
	case err == nil && v == o.Version && proto == Protocol:
		return c, Adopted, nil
	case err == nil:
		if err := c.Shutdown(ctx, o.Grace); err != nil {
			return nil, 0, fmt.Errorf("keeper %s (protocol %d): shutdown: %w", v, proto, err)
		}
		if err := waitGone(ctx, o.Dial, o.Grace+o.Wait); err != nil {
			return nil, 0, err
		}
		if err := startAndWait(ctx, c, o); err != nil {
			return nil, 0, err
		}
		return c, Replaced, nil
	case ctx.Err() != nil:
		return nil, 0, ctx.Err()
	default:
		if err := startAndWait(ctx, c, o); err != nil {
			return nil, 0, err
		}
		return c, Started, nil
	}
}

// Adoption is what Connect found.
type Adoption int

// Outcomes of Connect.
const (
	Adopted  Adoption = iota + 1 // a keeper of this version kept running
	Started                      // no keeper answered; a new one runs
	Replaced                     // a keeper of another version was shut down
)

func (a Adoption) String() string {
	switch a {
	case Adopted:
		return "adopted"
	case Started:
		return "started"
	case Replaced:
		return "replaced"
	}
	return "unknown"
}

const pollEvery = 50 * time.Millisecond

func startAndWait(ctx context.Context, c *Client, o ConnectOptions) error {
	if err := o.Start(); err != nil {
		return fmt.Errorf("keeper: start: %w", err)
	}
	deadline := time.Now().Add(o.Wait)
	for {
		ki, err := c.Version(ctx)
		v, proto := ki.Version, ki.Protocol
		if err == nil {
			if v != o.Version || proto != Protocol {
				return fmt.Errorf("keeper: started keeper reports %s (protocol %d), want %s (protocol %d)", v, proto, o.Version, Protocol)
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("keeper: no answer %s after start: %w", o.Wait, err)
		}
		time.Sleep(pollEvery)
	}
}

// waitGone waits until the keeper no longer accepts connections.
func waitGone(ctx context.Context, dial Dialer, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		conn, err := dial(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			return errors.New("keeper: old keeper still answers after shutdown")
		}
		time.Sleep(pollEvery)
	}
}
