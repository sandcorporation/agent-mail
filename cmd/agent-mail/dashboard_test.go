package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardTabsSkeleton(t *testing.T) {
	w := httptest.NewRecorder()
	dashboard(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	if !strings.Contains(body, `role="tablist"`) {
		t.Fatal("dashboard has no tablist")
	}
	// Tab order matters: 세션, 지표, 메일, 권한 요청, 질문, 설정.
	last := -1
	for _, name := range []string{"sessions", "metrics", "mail", "permissions", "questions", "settings"} {
		tab := `id="tab-` + name + `" data-tab="` + name + `" aria-controls="panel-` + name + `"`
		i := strings.Index(body, tab)
		if i < 0 {
			t.Fatalf("dashboard missing tab %q", name)
		}
		if i < last {
			t.Fatalf("tab %q out of order", name)
		}
		last = i
		if !strings.Contains(body, `id="panel-`+name+`" role="tabpanel" aria-labelledby="tab-`+name+`"`) {
			t.Fatalf("dashboard missing tabpanel %q", name)
		}
	}
	for _, want := range []string{
		`role="tab"`, "aria-selected", "function setTabCount(name,n)", "function focusQuestion(id)",
		"'hashchange'", "h.startsWith('q=')", "ArrowRight", "ArrowLeft", "tabLoaders", "id=\"reloadMail\"", "id=\"reloadSessions\"",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	for _, banned := range []string{"innerHTML", "insertAdjacentHTML"} {
		if strings.Contains(body, banned) {
			t.Fatalf("dashboard uses %s", banned)
		}
	}
}

func TestDashboardMetricsTab(t *testing.T) {
	w := httptest.NewRecorder()
	dashboard(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	for _, want := range []string{
		`<span class="tab-name">지표</span>`,
		"tabLoaders.metrics=loadMetrics",
		"api('metrics?days='+metricsDays)",
		`id="metDays7" data-days="7"`, `id="metDays30" data-days="30"`, `id="reloadMetrics"`,
		"document.createElementNS(SVG,tag)", "'http://www.w3.org/2000/svg'",
		"setTabCount('metrics',", "p.over_threshold", "t.error_s", "t.blocked_s",
		"function openThread(id)", "$('thread').value=id",
		`id="mFirstByRecipient"`, `id="mPending"`, `id="mThreads"`, `id="mStates"`, `id="mCurrent"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics tab missing %q", want)
		}
	}
}
