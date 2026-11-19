package hosting

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// limitService is a clocked service with node pc registered.
func limitService(t *testing.T) (*Service, *time.Time) {
	t.Helper()
	s, now := clockService(t)
	if err := s.RegisterLocalNode("pc", "sbn_pc"); err != nil {
		t.Fatal(err)
	}
	return s, now
}

func add(t *testing.T, s *Service, dir, class, desired string) wire.HostedSession {
	t.Helper()
	c := record()
	c.Directory, c.Class, c.Desired = dir, class, desired
	h, err := s.Create(c)
	if err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	return h
}

func TestClassDefaultsToExecuteAndIsChecked(t *testing.T) {
	s, _ := limitService(t)
	h := add(t, s, "/w/a", "", "")
	if h.Class != wire.ClassExecute || !h.AcceptingWork {
		t.Fatalf("defaults: class %q accepting %v", h.Class, h.AcceptingWork)
	}
	lead := wire.ClassLead
	if h, err := s.Update(h.ID, wire.HostedSessionChange{Class: &lead}); err != nil || h.Class != wire.ClassLead {
		t.Fatalf("update class: %+v %v", h, err)
	}
	if got, _ := s.Get(h.ID); got.Class != wire.ClassLead {
		t.Fatalf("stored class %q", got.Class)
	}
	bad := "boss"
	var invalid InvalidError
	if _, err := s.Update(h.ID, wire.HostedSessionChange{Class: &bad}); !errors.As(err, &invalid) {
		t.Fatalf("class %q: %v", bad, err)
	}
	c := record()
	c.Directory, c.Class = "/w/b", "boss"
	if _, err := s.Create(c); !errors.As(err, &invalid) {
		t.Fatalf("create class boss: %v", err)
	}
}

func TestFifthRunningSessionOnANodeIsRefused(t *testing.T) {
	s, _ := limitService(t)
	var ids []string
	for i := range 4 {
		ids = append(ids, add(t, s, "/w/"+strconv.Itoa(i), "", "").ID)
	}
	c := record()
	c.Directory = "/w/5"
	var capErr CapacityError
	if _, err := s.Create(c); !errors.As(err, &capErr) || capErr.Max != 4 || !strings.Contains(err.Error(), "SWITCHBOARD_HOSTING_MAX") {
		t.Fatalf("fifth create: %v", err)
	}
	// A stopped record is not refused, but starting it is.
	stopped := add(t, s, "/w/5", "", DesiredStopped)
	if _, err := s.SetDesired(stopped.ID, DesiredRunning); !errors.As(err, &capErr) {
		t.Fatalf("start beyond cap: %v", err)
	}
	if _, err := s.Restart(stopped.ID); !errors.As(err, &capErr) {
		t.Fatalf("restart of a stopped record beyond cap: %v", err)
	}
	if _, err := s.NewConversation(stopped.ID); !errors.As(err, &capErr) {
		t.Fatalf("new conversation of a stopped record beyond cap: %v", err)
	}
	// Starting one that already runs (clears a crash loop) is not refused.
	if _, err := s.SetDesired(ids[0], DesiredRunning); err != nil {
		t.Fatalf("start of a running record: %v", err)
	}
	// Stop one, and the stopped record may start.
	if _, err := s.SetDesired(ids[1], DesiredStopped); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetDesired(stopped.ID, DesiredRunning); err != nil {
		t.Fatalf("start within cap: %v", err)
	}
	// The cap is per node and follows the node's hello.
	if err := s.nodeSeen("pc", wire.NodeHello{Max: 6}); err != nil {
		t.Fatal(err)
	}
	add(t, s, "/w/6", "", "")
	if max, _ := s.NodeMax("pc"); max != 6 {
		t.Fatalf("node max %d, want 6", max)
	}
}

func TestCapRefusalIsAConflictOverTheAPI(t *testing.T) {
	s, _ := limitService(t)
	for i := range 4 {
		add(t, s, "/w/"+strconv.Itoa(i), "", "")
	}
	h := NewHub(s, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/admin/terminals", strings.NewReader(`{"node":"pc","base":"team/worker","profile":"p","directory":"/w/9"}`))
	h.AdminTerminalsAPI(w, r, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "cap of 4") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func accepting(t *testing.T, s *Service) map[string]bool {
	t.Helper()
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, h := range list {
		out[h.Class] = h.AcceptingWork
	}
	return out
}

func TestOperatorLimitStopsNewWorkForExecuteAndReviewOnly(t *testing.T) {
	s, _ := limitService(t)
	for i, class := range []string{wire.ClassLead, wire.ClassCoordinator, wire.ClassExecute, wire.ClassReview} {
		add(t, s, "/w/"+strconv.Itoa(i), class, "")
	}
	all := map[string]bool{"lead": true, "coordinator": true, "execute": true, "review": true}
	if got := accepting(t, s); !mapsEqual(got, all) {
		t.Fatalf("before the limit: %v", got)
	}
	l, err := s.SetLimit(true)
	if err != nil || !l.Limited || l.Source != LimitOperator {
		t.Fatalf("set: %+v %v", l, err)
	}
	want := map[string]bool{"lead": true, "coordinator": true, "execute": false, "review": false}
	if got := accepting(t, s); !mapsEqual(got, want) {
		t.Fatalf("limited: %v", got)
	}
	if l, err = s.SetLimit(false); err != nil || l.Limited || l.ClearedAt == nil {
		t.Fatalf("clear: %+v %v", l, err)
	}
	if got := accepting(t, s); !mapsEqual(got, all) {
		t.Fatalf("after the limit: %v", got)
	}
}

func mapsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func TestScreenLimitClearsAtResetAndOnOperatorOff(t *testing.T) {
	s, now := limitService(t)
	reset := now.Add(time.Hour).UnixMilli()
	if err := s.setNodeLimit("pc", wire.NodeLimit{Limited: true, Since: now.UnixMilli(), ResetAt: &reset}); err != nil {
		t.Fatal(err)
	}
	l, _ := s.Limit()
	if !l.Limited || l.Source != LimitScreen || l.ResetAt == nil || *l.ResetAt != reset {
		t.Fatalf("screen limit %+v", l)
	}
	*now = now.Add(time.Hour)
	if l, _ := s.Limit(); l.Limited {
		t.Fatalf("limited after the reset time: %+v", l)
	}

	// Operator off clears a showing that began before it.
	*now = now.Add(time.Minute)
	if err := s.setNodeLimit("pc", wire.NodeLimit{Limited: true, Since: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	if l, _ := s.Limit(); !l.Limited || l.ResetAt != nil {
		t.Fatalf("limit without reset %+v", l)
	}
	if _, err := s.SetLimit(false); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.Limit(); l.Limited {
		t.Fatalf("limited after operator off: %+v", l)
	}
}

func TestHubPublishesLimitClearedOnce(t *testing.T) {
	s, now := limitService(t)
	var kinds []string
	h := NewHub(s, func(k ...string) { kinds = append(kinds, k...) })
	if err := h.Sync(); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		n := 0
		for _, k := range kinds {
			if k == LimitClearedKind {
				n++
			}
		}
		return n
	}
	if _, err := h.SetLimit(true); err != nil {
		t.Fatal(err)
	}
	if count() != 0 || !slices.Contains(kinds, "terminals") {
		t.Fatalf("on: %v", kinds)
	}
	if _, err := h.SetLimit(false); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := h.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	if count() != 1 {
		t.Fatalf("operator off published %v", kinds)
	}

	// A node's screen limit clears at its reset time: once more.
	reset := now.Add(time.Minute).UnixMilli()
	h.receive(&nodeConn{name: "pc"}, wire.NodeFrame{Type: wire.NodeStateType, Limit: &wire.NodeLimit{Limited: true, Since: now.Add(time.Second).UnixMilli(), ResetAt: &reset}})
	*now = now.Add(2 * time.Second)
	if l, _ := s.Limit(); !l.Limited {
		t.Fatalf("node report did not limit: %+v", l)
	}
	*now = now.Add(time.Minute)
	for range 3 {
		if err := h.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	if count() != 2 {
		t.Fatalf("reset time published %v", kinds)
	}
	if l, _ := s.Limit(); l.ClearedAt == nil || *l.ClearedAt != now.UnixMilli() {
		t.Fatalf("cleared at %+v", l)
	}
}

func TestLimitAPI(t *testing.T) {
	s, _ := limitService(t)
	h := NewHub(s, nil)
	put := httptest.NewRecorder()
	h.AdminTerminalsAPI(put, httptest.NewRequest(http.MethodPut, "/api/admin/terminals/limit", strings.NewReader(`{"limited":true}`)), "limit")
	if put.Code != http.StatusOK || !strings.Contains(put.Body.String(), `"limited":true`) || !strings.Contains(put.Body.String(), `"source":"operator"`) {
		t.Fatalf("put %d %s", put.Code, put.Body)
	}
	get := httptest.NewRecorder()
	h.AdminTerminalsAPI(get, httptest.NewRequest(http.MethodGet, "/api/admin/terminals/limit", nil), "limit")
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"limited":true`) {
		t.Fatalf("get %d %s", get.Code, get.Body)
	}
}

func TestResumeOrderPutsQueuedWorkFirst(t *testing.T) {
	type item struct {
		id     string
		queued int64
		at     int64
	}
	items := []item{{"new2", 0, 50}, {"q2", 30, 20}, {"new1", 0, 40}, {"q1", 10, 30}}
	ResumeOrder(items, func(i item) WorkKey { return WorkKey{QueuedAt: i.queued, ArrivedAt: i.at} })
	var got []string
	for _, i := range items {
		got = append(got, i.id)
	}
	if strings.Join(got, " ") != "q1 q2 new1 new2" {
		t.Fatalf("order %v", got)
	}
}
