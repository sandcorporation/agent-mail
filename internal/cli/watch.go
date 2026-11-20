package cli

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// watchStart is `watch start [--attach] [--batch] ... -- CMD`: a job of kind
// watch (plain `watch` stays the inbox watch).
func watchStart(c Config, p parsedArgs) (int, error) {
	if len(p.pos) == 0 || p.pos[0] != "start" {
		return 0, usagef("watch: the only action is start (list, show, tail, stop and wait are job actions)")
	}
	batch := wire.DefaultWatchBatch
	if v := p.values["--batch"]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return 0, usagef("watch start: --batch takes a duration such as 10s or 1m")
		}
		batch = d
	}
	return startJob(c, p, wire.JobWatch, batch, p.bool("--attach"))
}

// lineBatcher splits a watch's output into lines and holds the lines of the
// open batch window: every line counts, the first ones within the mail cap
// are kept. Write never blocks on the server.
type lineBatcher struct {
	mu     sync.Mutex
	part   []byte // an unterminated last line
	lines  []string
	size   int
	count  int
	wake   chan struct{} // a line arrived in an empty batch
	closed chan struct{}
	once   sync.Once
}

func newLineBatcher() *lineBatcher {
	return &lineBatcher{wake: make(chan struct{}, 1), closed: make(chan struct{})}
}

func (b *lineBatcher) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.part = append(b.part, p...)
	for {
		i := bytes.IndexByte(b.part, '\n')
		if i < 0 {
			break
		}
		b.add(strings.TrimSuffix(string(b.part[:i]), "\r"))
		b.part = b.part[i+1:]
	}
	if len(b.part) > 2*wire.MaxJobTailBytes { // a runaway line counts once
		b.add(string(b.part))
		b.part = nil
	}
	return len(p), nil
}

// add takes one line; the caller holds mu.
func (b *lineBatcher) add(l string) {
	if b.count == 0 {
		select {
		case b.wake <- struct{}{}:
		default:
		}
	}
	b.count++
	if len(b.lines) < wire.MaxJobTailLines && b.size+len(l)+1 <= wire.MaxJobTailBytes {
		b.lines = append(b.lines, l)
		b.size += len(l) + 1
	}
}

// Close ends the output: an unterminated last line counts, and the batch
// left is sent at once.
func (b *lineBatcher) Close() {
	b.once.Do(func() {
		b.mu.Lock()
		if len(b.part) > 0 {
			b.add(string(b.part))
			b.part = nil
		}
		b.mu.Unlock()
		close(b.closed)
	})
}

func (b *lineBatcher) take() (string, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	lines, count := strings.Join(b.lines, "\n"), b.count
	b.lines, b.size, b.count = nil, 0, 0
	return lines, count
}

// runBatches sends one batch per window: the first line of an empty batch
// opens a window of length window (after is the clock), its end sends what
// arrived. It returns after Close, once the rest is sent.
func runBatches(b *lineBatcher, window time.Duration, after func(time.Duration) <-chan time.Time, send func(lines string, count int)) {
	for {
		select {
		case <-b.wake:
			select {
			case <-after(window):
			case <-b.closed:
			}
		case <-b.closed:
		}
		if lines, count := b.take(); count > 0 {
			send(lines, count)
		}
		select {
		case <-b.closed:
			if lines, count := b.take(); count > 0 {
				send(lines, count)
			}
			return
		default:
		}
	}
}

// watchSender posts a watch's batches, numbered from 1, unless a client is
// attached to the watch on this machine (it reads the output file itself).
func watchSender(c Config, id int64, dir string) func(string, int) {
	var seq int64
	return func(lines string, count int) {
		seq++
		if liveWaiter(dir) {
			return
		}
		q := wire.JobLines{Seq: seq, Count: count, Lines: lines}
		path := fmt.Sprintf("/api/jobs/%d/lines", id)
		pause := 500 * time.Millisecond
		for i := 0; ; i++ {
			_, err := slotCall(c, "POST", path, q)
			if err == nil || i+1 >= 5 {
				if err != nil {
					log.Printf("job %d: watch batch %d failed: %v", id, seq, err)
				}
				return
			}
			time.Sleep(pause)
			pause *= 2
		}
	}
}
