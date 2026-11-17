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
	// Tab order matters: 세션, 메일, 권한 요청, 질문, 설정.
	last := -1
	for _, name := range []string{"sessions", "mail", "permissions", "questions", "settings"} {
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
