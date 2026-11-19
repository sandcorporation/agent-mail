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
	// Tab order matters: 운영 (진행, 세션, 메일, 보고), 응답 (질문, 권한 요청),
	// 관리 (지표, 터미널, 설정).
	last := -1
	for _, name := range []string{"progress", "sessions", "mail", "reports", "questions", "permissions", "metrics", "terminals", "settings"} {
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
		`role="tab"`, "aria-selected", "function setTabCount(name,n)", "history.replaceState(null,'',detailHash('questions',id,'answer'));",
		"'hashchange'", "h.startsWith('q=')", "ArrowRight", "ArrowLeft", "ArrowDown", "ArrowUp", "tabLoaders", "id=\"reloadMail\"", "id=\"reloadSessions\"",
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
		`<span class="tab-name sc-sidenav-label">지표</span>`,
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
		`<span class="tab-name sc-sidenav-label">진행</span>`,
		`id="tab-progress" data-tab="progress" aria-controls="panel-progress" aria-selected="true"`,
		`id="panel-progress" role="tabpanel" aria-labelledby="tab-progress" tabindex="0" class="card">`,
		`id="panel-sessions" role="tabpanel" aria-labelledby="tab-sessions" tabindex="0" class="card" hidden>`,
		"tabLoaders.progress=loadProgress", "api('progress')", `id="reloadProgress"`, `id="progress"`,
		"showTab(visibleTabs().includes(h)?h:'progress')",
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

func TestDashboardQuestionWaitingDelivery(t *testing.T) {
	w := httptest.NewRecorder()
	Page(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	for _, want := range []string{"if(q.delivery==='waiting')h.append(node('span','전달 대기','pill waiting'));", ".pill.waiting{"} {
		if !strings.Contains(body, want) {
			t.Fatalf("question card missing %q", want)
		}
	}
}

// The 터미널 tab (terminal hosting, #186) is hidden in the page and shown
// only when the bootstrap call reports hosting:true; with hosting off it is
// absent from the tab bar, keyboard order and routing.
func TestDashboardTerminalsTabOnlyWithHosting(t *testing.T) {
	w := httptest.NewRecorder()
	Page(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	tab := regexp.MustCompile(`<button[^>]*id="tab-terminals"[^>]*>`).FindString(body)
	if tab == "" || !strings.Contains(tab, " hidden") {
		t.Fatalf("terminals tab button %q is not hidden by default", tab)
	}
	if !strings.Contains(body, `id="panel-terminals" role="tabpanel" aria-labelledby="tab-terminals" tabindex="0" class="card" hidden>`) {
		t.Fatal("terminals panel missing or shown by default")
	}
	for _, want := range []string{
		`$('tab-terminals').hidden=!hostingOn;`,
		`function visibleTabs(){return tabNames.filter(n=>n!=='terminals'||hostingOn)}`,
		`showTab(visibleTabs().includes(h)?h:'progress');`,
		`terminals:loadTerminals}`,
		`tabLoaders.terminals=loadTerminals;`,
		`api('terminals')`,
		`confirm(`,
		`<button type="button" data-view="nodes" aria-pressed="false" hidden>노드</button>`,
		`setViewButton('nodes').hidden=!hostingOn;`,
		`if(!setViewHash[v]||(v==='nodes'&&!hostingOn))v='qsettings';`,
		`api('nodes')`,
		`apiSend('DELETE','nodes/'+encodeURIComponent(v.name))`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "innerHTML") {
		t.Error("dashboard renders HTML from strings")
	}
}

// Hosted sessions: the Terminals tab shows the slot role and presence, and
// the Sessions tab marks a session a hosted record holds with a link to its
// terminal detail (#terminals/<id>/screen), rendered as text.
func TestDashboardSessionsMarkHostedSessions(t *testing.T) {
	w := httptest.NewRecorder()
	Page(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	for _, want := range []string{
		`const hosted=hostingOn?await hostedBySlot():new Map();`,
		`if(v.slot&&v.slot.role)m.set(v.slot.role,v)`,
		`const h=hosted.get(v.role);`,
		`node('a','터미널','hosted');a.href=detailHash('terminals',h.id,'screen')`,
		`if(h.startsWith('terminal=')){`,
		`history.replaceState(null,'',detailHash('terminals',id,'screen'));route();`,
		`v.slot&&v.slot.role?v.slot.role:v.base`,
		`'offline (process alive)':'오프라인 (프로세스 살아 있음)'`,
		`const pr=v.presence||'',sub=[termPresence[pr]||pr,`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
}

// Running background sub-agents (#215) show next to the state, as text, in
// the 진행 cards, the 세션 list and the session's detail.
func TestDashboardShowsSubagents(t *testing.T) {
	w := httptest.NewRecorder()
	Page(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	for _, want := range []string{
		"function subagentNote(n){return n>0?'서브 에이전트 '+n+'개 작업 중':''}",
		"const s=node('span','서브 '+v.subagents,'subs');s.title=subagentNote(v.subagents);tr.children[1].append(' · ',s)",
		"['서브 에이전트',v.subagents>0?subagentNote(v.subagents):'-']",
		"node('span',subagentNote(v.subagents),'subs')]:[]",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
}

// The frame is the design system's app shell: side menu in three groups,
// top bar with breadcrumb, live indicator and the signed-in admin, and the
// tab panels in the page area. The stylesheet is inlined, never linked, so
// the page references no /assets/ path while hosting is off.
func TestDashboardDesignFrame(t *testing.T) {
	w := httptest.NewRecorder()
	Page(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	css := DesignCSS()
	if !strings.HasPrefix(css, "/* Copied from github.com/sandcorporation/sandcorp-design, tag v") || !strings.Contains(css, ".sc-sidenav-item{") {
		t.Fatalf("sandcorp.css header or content missing: %.120q", css)
	}
	if !strings.Contains(body, "<style>\n"+css+"</style>") || strings.Contains(body, "/assets/") || strings.Contains(body, "<link") {
		t.Fatal("design stylesheet not inlined once ahead of the page styles")
	}
	if !strings.HasPrefix(productName, productMark) {
		t.Fatalf("mark %q is not the first letter of %q", productMark, productName)
	}
	order := []string{
		`<div class="sc-app">`, `<aside class="sc-sidenav" aria-label="주 메뉴">`, `<h1>` + headerText + `</h1>`,
		`<div id="tabs" class="tabs" role="tablist" aria-label="탭">`,
		`>운영</div>`, `id="tab-progress"`, `id="tab-sessions"`, `id="tab-mail"`,
		`>응답</div>`, `id="tab-questions"`, `id="tab-permissions"`,
		`>관리</div>`, `id="tab-metrics"`, `id="tab-terminals"`, `id="tab-settings"`,
		`<div class="sc-app-main" id="appMain">`, `<header class="sc-topbar">`, `<li id="crumbTab" aria-current="page">진행</li>`,
		`id="live"`, `id="whoName"`, `id="logout"`, `<main class="sc-app-page">`, `id="panel-progress"`, `</main>`, `<script>`,
	}
	last := -1
	for _, want := range order {
		i := strings.Index(body, want)
		if i < last {
			t.Fatalf("frame missing or out of order: %q", want)
		}
		last = i
	}
	// 응답 items keep a waiting dot when the menu collapses to icons.
	for _, name := range []string{"questions", "permissions"} {
		tab := regexp.MustCompile(`<button[^>]*id="tab-` + name + `"[^>]*>`).FindString(body)
		if !strings.Contains(tab, " data-wait ") {
			t.Fatalf("%s tab has no data-wait: %q", name, tab)
		}
	}
	for _, want := range []string{
		".sc-sidenav-item[data-wait][data-nonzero] .sc-sidenav-count{display:block",
		"t.toggleAttribute('data-nonzero',!!n);",
		"$('crumbTab').textContent=t.querySelector('.tab-name').textContent;",
		"--st-idle:var(--color-success);", "--st-error:var(--color-danger);", "--st-blocked:var(--color-warning);",
		"--st-busy:var(--color-accent);", "--st-offline:var(--color-muted);",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("frame missing %q", want)
		}
	}
	// The page's own colours all come from the design tokens.
	own := dashboardHTML[strings.Index(dashboardHTML, "<style>"):strings.Index(dashboardHTML, "</style>")]
	if m := regexp.MustCompile(`:#[0-9a-fA-F]{3,6}\b|--(bg|surface|surface-2|border|border-strong|text|muted|accent|accent-soft|idle|busy|compacting|error|blocked|offline|s[1-6]|r|r-sm|mono)[:)]`).FindString(own); m != "" {
		t.Errorf("page styles define their own colour or spacing %q", m)
	}
}

// List + right detail panel (#229): one panel after the page area, the hash
// #<tab>/<id>/<sub> opens it (reload and back/forward restore it), Esc closes
// it, focus goes to its title on open and back to the row on close, and below
// 720px it covers the screen with a back button. 세션 and 메일 use it.
func TestDashboardDetailPanel(t *testing.T) {
	w := httptest.NewRecorder()
	Page(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	order := []string{
		`<main class="sc-app-page">`, `</main>`,
		`<aside id="detail" class="detail" aria-labelledby="detailTitle" hidden>`,
		`<button type="button" id="detailBack" class="detail-back">`, `<h2 id="detailTitle" tabindex="-1">`,
		`id="detailBadge" class="sc-badge"`, `id="detailClose"`, `<nav id="detailTabs" class="sc-tabs"`, `id="detailBody"`, `<script>`,
	}
	last := -1
	for _, want := range order {
		i := strings.Index(body, want)
		if i < last {
			t.Fatalf("detail panel missing or out of order: %q", want)
		}
		last = i
	}
	for _, want := range []string{
		// Layout: 440px right column, sticky top bar, full screen below 720px.
		".sc-app-main.has-detail{grid-template-columns:minmax(0,1fr) 440px}",
		".sc-topbar{position:sticky;top:0;",
		"@media (max-width:720px){\n.sc-app-main.has-detail{grid-template-columns:minmax(0,1fr)}\n.detail{position:fixed;inset:0;",
		".detail-back{display:inline-block}",
		// Hash restore: #<tab>/<id>/<sub>.
		"function detailHash(tab,id,sub){return '#'+tab+'/'+encodeURIComponent(id)+(sub?'/'+sub:'')}",
		"if(parts.length>1&&details[parts[0]]&&visibleTabs().includes(parts[0])&&parts[1]){",
		"showTab(parts[0]);showDetail(parts[0],id,parts[2]||'');",
		"  hideDetail();\n",
		// Esc closes; focus to the title on open, back to the row on close.
		"if(e.key==='Escape'&&detailCur&&!e.defaultPrevented){e.preventDefault();closeDetail()}",
		"$('detailTitle').focus({preventScroll:true});",
		"if(back&&tr&&curTab===c.tab)tr.focus();",
		// Rows: Tab-reachable, Enter opens, the open row is marked.
		"tr.dataset.detailId=id;tr.tabIndex=0;",
		"if(e.target===tr&&(e.key==='Enter'||e.key===' ')){e.preventDefault();openDetail(tab,id)}",
		"table.list tr[aria-current=true] td{",
		"$('detailBack').textContent='← '+details[tab].list;",
		// Live updates redraw the open item and keep the selection and sub-tab.
		"if(detailCur&&(details[detailCur.tab].kinds||[detailCur.tab]).includes(k))liveDetail=true;",
		"if(liveDetail){liveDetail=false;drawDetail()}",
		"if(seq!==detailSeq)return;",
		// 세션 and 메일 register.
		"registerDetail('sessions',{", "subs:it=>[['status','상태'],['mail','메일']].concat(it.h?[['terminal','터미널']]:[]),",
		"registerDetail('mail',{", "subs:()=>[['body','본문'],['thread','스레드']],",
		"box.append(dl(pairs),node('pre',m.body));",
		"if(f.id)openDetail('mail',String(f.id),'body');else selectTab('mail');",
		`<thead><tr><th>역할</th><th>상태</th><th class="num">컨텍스트</th><th>마지막 턴</th><th>기계</th><th>라벨</th></tr></thead>`,
		`<thead><tr><th class="num">#</th><th>제목</th><th>보낸 쪽 → 받는 쪽</th><th>상태</th><th>시각</th></tr></thead>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail panel missing %q", want)
		}
	}
	// The six mail filters stay.
	for _, id := range []string{"box", "role", "status", "search", "thread", "task"} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Fatalf("mail filter %q gone", id)
		}
	}
}

// 터미널 (#231): list + detail like 세션 and 메일. The list has name,
// profile, node, state, permissions, restarts and last start with state
// chips; the add form is a drawer; the detail has 화면 (one live screen that
// moves between redraws and the 세션 detail), 기록 and 설정, the actions
// with their confirmations, and 넓히기 with an Esc order: a typing terminal
// takes Esc, then 넓히기 returns, then the panel closes.
func TestDashboardTerminalsDetail(t *testing.T) {
	w := httptest.NewRecorder()
	Page(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	for _, want := range []string{
		`<table id="terminalsTbl" class="stack list"><thead><tr><th>이름</th><th>프로필</th><th>노드</th><th>상태</th><th>권한</th><th class="num">재시작</th><th>마지막 시작</th></tr></thead>`,
		`<div class="chips" id="termChips" role="group"`,
		`<dialog id="termAdd" class="sc-drawer" aria-labelledby="termAddTitle">`,
		`<form id="termForm" class="settings-form termform">`,
		"$('termAddOpen').onclick=()=>$('termAdd').showModal();",
		"$('termAdd').addEventListener('keydown',e=>{if(e.key==='Escape'){e.preventDefault();$('termAdd').close()}});",
		"detailRow(tr,'terminals',v.id);",
		"registerDetail('terminals',{",
		"subs:()=>[['screen','화면'],['log','기록'],['settings','설정']],",
		"add('지우기',()=>{if(confirm(",
		"add('새 대화',()=>{if(confirm(",
		"node('p','바꾼 설정은 다음 시작부터 적용','muted')",
		"api('terminals/'+encodeURIComponent(v.id)+'/log')",
		// One screen, kept across redraws, closed when the detail stops showing it.
		`<div id="termHold" hidden><div id="termPane" class="termpane">`,
		"function termGc(){if(!$('detailBody').contains(tp('termPane')))termClose()}",
		"  termGc();\n}",
		"$('detailBody').replaceChildren();termGc();",
		"box.append(dl([['노드',it.h.node],['폴더',it.h.directory],['터미널',a]]),termPaneFor(it.h));",
		// 넓히기 and the Esc order.
		".sc-app-main.has-detail.detail-wide{grid-template-columns:minmax(0,1fr)}",
		"if(termView&&termView.mode==='input'&&tp('termScreen').contains(e.target))return;\n  e.preventDefault();termWiden(false);\n},true);",
		"tp('termScreen').addEventListener('keydown',e=>{if(e.key==='Escape'&&termView&&termView.mode==='input')e.preventDefault()});",
		"t.term.attachCustomKeyEventHandler(e=>!(e.key==='Escape'&&t.mode!=='input'));",
		// The input path and its ticket stay as they were.
		"tk=await apiSend('POST','terminals/'+encodeURIComponent(t.id)+'/ticket',{mode:t.mode})",
		"function termSend(t,m){if(t.mode==='input'&&t.ws&&t.ws.readyState===1)t.ws.send(JSON.stringify(m))}",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("terminals detail missing %q", want)
		}
	}
	for _, gone := range []string{`id="termView"`, `id="termBack"`, "termFocus"} {
		if strings.Contains(body, gone) {
			t.Errorf("old terminals view left: %q", gone)
		}
	}
}

// 진행, 질문 and 권한 요청 use the list + detail panel (#230): 진행 starts
// with 대기 (질문, 권한, 오류, 지시 in that order, oldest first within a
// kind), then the services under three chips; every row opens a detail.
func TestDashboardWaitAndDetails(t *testing.T) {
	w := httptest.NewRecorder()
	Page(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	order := []string{
		`<h3 class="sechead">대기 <span id="waitCount"`,
		`<table id="waitTbl" class="stack list cards"><thead><tr><th>종류</th><th>항목</th><th>역할</th><th class="num">지난 시간</th></tr></thead><tbody id="wait"></tbody></table>`,
		`<h3 class="sechead">서비스</h3>`, `id="svcChips"`,
		`<table id="progressTbl" class="stack list cards"><thead><tr><th>서비스</th><th>상태</th><th class="num">세션</th><th class="num">열린 지시</th><th>마지막 소식</th></tr></thead><tbody id="progress"></tbody></table>`,
		`id="panel-sessions"`,
	}
	last := -1
	for _, want := range order {
		i := strings.Index(body, want)
		if i < last {
			t.Fatalf("progress list missing or out of order: %q", want)
		}
		last = i
	}
	for _, want := range []string{
		// 대기: kinds in order, oldest first, the counts the menu shows.
		"const waitKinds=[['q','질문','warning'],['p','권한','info'],['s','오류','danger'],['d','지시','neutral'],['m','멈춤','warning']];",
		"for(const x of p.stalled||[])by.m.push(",
		"for(const [k,label,tone] of waitKinds)for(const x of by[k].sort((a,b)=>b.age-a.age))",
		"api('questions?status=pending&limit=500'),api('permissions?status=pending')",
		"for(const x of p.open_directives)if(x.over_threshold)by.d.push(",
		"detailRow(tr,'progress',x.kind+':'+x.id);",
		"const svcChips=[['all','전체',()=>true],['problem','문제 있음',p=>/^(error|blocked)$/.test(p.badge)],['stale','오래된 지시',p=>p.open_directives.some(d=>d.over_threshold)]];",
		"detailRow(tr,'progress','svc:'+p.service);",
		// Each 대기 kind opens its own detail.
		"const progressKinds={q:'questions',p:'permissions',s:'sessions',d:progressDirective,m:progressDirective,svc:progressService};",
		"registerDetail('progress',{",
		"kinds:['progress','questions','permissions','sessions','mail'],",
		"subs:x=>[['sessions','세션 '+x.p.sessions.length],['directives','열린 지시 '+x.p.open_directives.length],['news','마지막 소식']],",
		// 지시: thread first, 닫기 through the admin close API.
		"subs:()=>[['thread','스레드'],['body','본문']],",
		"apiSend('POST','messages/'+encodeURIComponent(x.m.id)+'/close')",
		// 질문: 답하기 / 관련 결정; the form survives live redraws; a late 409 is one line.
		"registerDetail('questions',{",
		"subs:x=>[['answer',x.q.status==='pending'&&x.q.can_answer?'답하기':'답'],['decisions','관련 결정 '+x.decs.length]],",
		"if(!f){f=answerForm(q);qForms.set(q.id,f)}",
		"qNotes.set(q.id,'이미 답했거나 거둔 질문입니다(409)');",
		"detailRow(tr,'questions',q.id);",
		// 권한 요청: 대기 / 지난 기록 lists, detail with allow and deny.
		"registerDetail('permissions',{",
		"api('permissions/'+encodeURIComponent(id))",
		"detailRow(tr,'permissions',v.id);",
		"if(v.status==='pending'&&v.can_answer)box.append(permAnswerForm(v));",
		// Below 600px 대기 and service rows are two-line cards.
		"table.cards td.cardbr{display:block;flex-basis:100%;height:0}",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	for _, banned := range []string{"function progressCard(", "function focusQuestion(", "function permPendingCard("} {
		if strings.Contains(body, banned) {
			t.Fatalf("old card view left: %q", banned)
		}
	}
}

// #223: 설정 is a list + detail with the views 질문 설정 / 역할 / 노드 / 다시
// 알림. Every earlier action stays (question setting add, edit and delete;
// role revoke and restore; node revoke, each behind its confirm), and 다시
// 알림 reads GET /api/admin/settings.
func TestDashboardSettingsDetail(t *testing.T) {
	w := httptest.NewRecorder()
	Page(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	for _, want := range []string{
		`<div class="viewswitch" id="setViews" role="group" aria-label="설정 고르기"><button type="button" data-view="qsettings" aria-pressed="true">질문 설정</button><button type="button" data-view="roles" aria-pressed="false">역할</button><button type="button" data-view="nodes" aria-pressed="false" hidden>노드</button><button type="button" data-view="reminders" aria-pressed="false">다시 알림</button></div>`,
		`<table id="settingsTbl" class="stack list cards">`,
		`<table id="rolesTbl" class="stack list cards">`,
		`<table id="nodesTbl" class="stack list cards">`,
		`<dialog id="setAdd" class="sc-drawer" aria-labelledby="setAddTitle">`,
		"const setViewHash={qsettings:'settings',roles:'settings-roles',nodes:'settings-nodes',reminders:'settings-reminders'};",
		"registerDetail('settings',{",
		"detailRow(tr,'settings','qs:'+v.target);",
		"detailRow(tr,'settings','role:'+v.role);",
		"detailRow(tr,'settings','node:'+v.name);",
		"back:id=>setViewHash[setKindView[id.slice(0,id.indexOf(':'))]]||'settings',",
		"function closeDetail(){if(detailCur){const b=details[detailCur.tab].back;location.hash=b?b(detailCur.id):detailCur.tab}}",
		"if(parts[0]==='settings')showSettingsView(setKindView[id.slice(0,id.indexOf(':'))]);",
		"apiSend('PUT','question-settings/'+targetPath(v.target),{can_ask:canAsk.checked,watchers:watchers,discord:discord.checked})",
		"apiSend('PUT','question-settings/'+targetPath(target),{can_ask:$('setCanAsk').checked,watchers:watchers,discord:$('setDiscord').checked})",
		"if(!confirm(v.target+' 설정을 지울까요?'))return;",
		"apiSend('DELETE','question-settings/'+targetPath(v.target))",
		"if(v.target!=='default'){",
		"apiSend('POST','roles/'+targetPath(v.role)+(revoke?'/revoke':'/restore'))",
		"confirm(revoke?v.role+' 역할을 끊을까요? 이 토큰으로 접속한 세션이 모두 끊깁니다.':v.role+' 역할을 되살릴까요?')",
		"confirm(v.name+' 노드를 끊을까요? 연결이 바로 끊기고 이 노드의 토큰은 다시 쓸 수 없습니다.')",
		"apiSend('DELETE','nodes/'+encodeURIComponent(v.name))",
		"const s=await api('settings');",
		"$('setAdd').addEventListener('keydown',e=>{if(e.key==='Escape'){e.preventDefault();$('setAdd').close()}});",
		"settings:loadSettingsView,",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	for _, gone := range []string{`id="nodesBox"`, `id="rolesMsg"`, `id="nodesMsg"`, `function loadSettings(`} {
		if strings.Contains(body, gone) {
			t.Errorf("dashboard still has %q", gone)
		}
	}
}
