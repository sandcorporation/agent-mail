package dashboard

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestDashboardTabsSkeleton(t *testing.T) {
	w := httptest.NewRecorder()
	Page(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	if !strings.Contains(body, `role="tablist"`) {
		t.Fatal("dashboard has no tablist")
	}
	// Tab order matters: 진행, 세션, 지표, 메일, 권한 요청, 질문, 설정.
	last := -1
	for _, name := range []string{"progress", "sessions", "metrics", "mail", "permissions", "questions", "settings"} {
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
	Page(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	for _, want := range []string{
		`<span class="tab-name">지표</span>`,
		"tabLoaders.metrics=loadMetrics",
		"api('metrics?days='+metricsDays)",
		`id="metDays7" data-days="7"`, `id="metDays30" data-days="30"`, `id="reloadMetrics"`,
		"document.createElementNS(SVG,tag)", "'http://www.w3.org/2000/svg'",
		"setTabCount('metrics',", "p.over_threshold", "t.error_s", "t.blocked_s",
		"function openThread(id)", "$('thread').value=f.thread||''",
		`id="mFirstByRecipient"`, `id="mPending"`, `id="mThreads"`, `id="mStates"`, `id="mCurrent"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics tab missing %q", want)
		}
	}
}

func TestDashboardProgressTab(t *testing.T) {
	w := httptest.NewRecorder()
	Page(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	for _, want := range []string{
		`<span class="tab-name">진행</span>`,
		`id="tab-progress" data-tab="progress" aria-controls="panel-progress" aria-selected="true"`,
		`id="panel-progress" role="tabpanel" aria-labelledby="tab-progress" tabindex="0" class="card">`,
		`id="panel-sessions" role="tabpanel" aria-labelledby="tab-sessions" tabindex="0" class="card" hidden>`,
		"tabLoaders.progress=loadProgress", "api('progress')", `id="reloadProgress"`, `id="progress"`,
		"showTab(tabNames.includes(h)?h:'progress')",
		"function openMessage(id)", `id="task"`, "task:task,id:id",
		"setTabCount('progress',", "d.over_threshold", "'세션 없음'",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("progress tab missing %q", want)
		}
	}
}

// TestDashboardNoUsageCopy keeps explanatory copy out of the tab panels: a
// static <p> in a panel may only be a terse empty state, and the script must
// not build usage sentences. Hints belong in title tooltips.
func TestDashboardNoUsageCopy(t *testing.T) {
	w := httptest.NewRecorder()
	Page(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	html := body[:strings.Index(body, "<script>")]
	staticP := regexp.MustCompile(`<p( [^>]*)?>([^<]+)</p>`)
	for _, panel := range strings.Split(html, `<section id="panel-`)[1:] {
		for _, m := range staticP.FindAllStringSubmatch(panel, -1) {
			if m[1] != ` class="empty"` || len([]rune(m[2])) > 12 {
				t.Errorf("panel %s has copy %q", panel[:strings.Index(panel, `"`)], m[0])
			}
		}
	}
	for _, banned := range []string{"봅니다", "보여 줍니다", "누르면", "강조합니다", "이 탭은", "돌아갑니다", "<!--"} {
		if strings.Contains(body, banned) {
			t.Errorf("dashboard has usage copy %q", banned)
		}
	}
}

func TestDashboardHeaderFromOneConstant(t *testing.T) {
	if headerText != "Switchboard" {
		t.Fatalf("headerText = %q", headerText)
	}
	w := httptest.NewRecorder()
	Page(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	for _, want := range []string{"<title>" + productName + "</title>", "<h1>" + headerText + "</h1>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
}

// Collaborator groups and invites have no dashboard view (#140).
func TestDashboardHasNoCollaboratorsView(t *testing.T) {
	w := httptest.NewRecorder()
	Page(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	for _, banned := range []string{"collaborators", "협업자", "invite", "groups/", "묶음"} {
		if strings.Contains(body, banned) {
			t.Fatalf("dashboard still has %q", banned)
		}
	}
}

func TestDashboardPermissionsTab(t *testing.T) {
	w := httptest.NewRecorder()
	Page(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	for _, want := range []string{
		"api('permissions?status=pending')", "api('permissions?status=closed&limit=200')",
		"apiSend('POST','permissions/'+encodeURIComponent(v.id)+'/answer',{decision:decision,note:note.value})",
		"tabLoaders.permissions=loadPermissions", "id=\"permPending\"", "id=\"permHistory\"",
		"['요청한 역할',v.requester]", "['답한 쪽',v.answered_by]", "답은 바꿀 수 없습니다.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("permissions tab missing %q", want)
		}
	}
	if strings.Contains(body, "준비 중") {
		t.Fatal("permissions tab is still a placeholder")
	}
}

func TestDashboardLiveUpdates(t *testing.T) {
	w := httptest.NewRecorder()
	Page(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	for _, want := range []string{
		"new URL('/api/admin/live',location.href)", "u.protocol=location.protocol==='https:'?'wss:':'ws:'",
		"u.searchParams.set('boot',liveBoot);u.searchParams.set('after',String(liveSeq))",
		"liveDelay=Math.min(liveDelay*2,30000)", "m.type==='hello'", "m.type==='change'", "liveConnect();",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("live updates missing %q", want)
		}
	}
	// Every tab has a live reload.
	for _, name := range []string{"progress", "sessions", "metrics", "mail", "permissions", "questions", "settings"} {
		if !regexp.MustCompile(`liveReload=\{[^}]*\b` + name + `:`).MatchString(body) {
			t.Fatalf("no live reload for %s", name)
		}
	}
}
