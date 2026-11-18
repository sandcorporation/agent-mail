// Package dashboard is the admin web page (one HTML document with its CSS and
// JS).
package dashboard

import (
	"io"
	"net/http"

	"github.com/sandcorporation/agent-mail/internal/config"
)

// Page serves the admin overview. It only calls same-origin /api/admin/*
// endpoints, sends no Authorization header and stores nothing in the browser.
// Its only writes are question answers, question settings and role revokes
// and restores, sent with the
// admin write guard headers. Message and question fields are rendered as text
// nodes, never as HTML.
func Page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, dashboardHTML)
}

// headerText is the only place the dashboard header lives; the product name
// is config.ProductName.
const (
	productName = config.ProductName
	headerText  = productName
)

// dashboardHTML is the whole page. Design tokens (colours, spacing, radius)
// sit on :root and are redefined for prefers-color-scheme: dark; state colours
// (--idle, --busy, --compacting, --error, --blocked, --offline) are shared by
// session text, rows, progress badges and question pills.
const dashboardHTML = `<!doctype html>
<html lang="ko">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>` + productName + `</title>
<style>
:root{color-scheme:light dark;
--bg:#f4f6f9;--surface:#fff;--surface-2:#f0f3f7;--border:#dfe4ec;--border-strong:#c5cdd9;
--text:#18202e;--muted:#5f6b7c;--accent:#2457c5;--accent-fg:#fff;--accent-soft:#e7eefc;--focus:#2457c5;
--idle:#1f7a3a;--idle-bg:#e5f4ea;--busy:#2457c5;--busy-bg:#e7eefc;--compacting:#7a3fb0;--compacting-bg:#f1e8fa;
--error:#c0302b;--error-bg:#fceaea;--blocked:#a35a00;--blocked-bg:#fdf1e0;--offline:#7b8594;--offline-bg:#eef0f3;
--s1:.25rem;--s2:.5rem;--s3:.75rem;--s4:1rem;--s5:1.5rem;--s6:2rem;--r:8px;--r-sm:5px;
--mono:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
@media (prefers-color-scheme:dark){:root{
--bg:#0f1319;--surface:#171c24;--surface-2:#1f2630;--border:#2b3340;--border-strong:#3b4555;
--text:#e4e8ef;--muted:#97a1b0;--accent:#7aa5ff;--accent-fg:#0f1319;--accent-soft:#1d2a44;--focus:#7aa5ff;
--idle:#5cc77f;--idle-bg:#16301f;--busy:#7aa5ff;--busy-bg:#1d2a44;--compacting:#c69bf2;--compacting-bg:#2c2040;
--error:#ff7b74;--error-bg:#3a1c1c;--blocked:#f0ad4e;--blocked-bg:#36280f;--offline:#8a93a1;--offline-bg:#232932}}
*,*::before,*::after{box-sizing:border-box}
body{margin:0 auto;max-width:1200px;padding:var(--s4) var(--s4) var(--s6);background:var(--bg);color:var(--text);font:15px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,"Noto Sans KR","Apple SD Gothic Neo","Malgun Gothic",sans-serif;-webkit-text-size-adjust:100%}
h1{font-size:1.2rem;font-weight:650;margin:0;letter-spacing:-.01em}
h2{font-size:1.05rem;font-weight:650;margin:0}
h3{font-size:.98rem;font-weight:650}
h4{font-size:.85rem;font-weight:600;color:var(--muted)}
a{color:var(--accent)}
input,button,select{font:inherit;color:inherit}
input,select{padding:.4rem .55rem;margin:0;border:1px solid var(--border-strong);border-radius:var(--r-sm);background:var(--surface);min-width:0}
input[type=checkbox],input[type=radio]{accent-color:var(--accent)}
button,a.button{padding:.4rem .75rem;margin:0;border:1px solid var(--border-strong);border-radius:var(--r-sm);background:var(--surface);color:var(--text);cursor:pointer;line-height:1.4}
button:hover,a.button:hover{border-color:var(--accent);color:var(--accent)}
button[type=submit]{background:var(--accent);border-color:var(--accent);color:var(--accent-fg)}
button[type=submit]:hover{color:var(--accent-fg);filter:brightness(1.08)}
button:disabled{opacity:.55;cursor:default}
:focus-visible{outline:2px solid var(--focus);outline-offset:2px}
.row{display:flex;gap:var(--s2);align-items:center;flex-wrap:wrap}
.card{background:var(--surface);border:1px solid var(--border);border-radius:var(--r);padding:var(--s4);margin:var(--s3) 0;overflow-wrap:anywhere}
.card h3{margin:0 0 var(--s1)}
.card.urgent{border-left:4px solid var(--error)}
.badge{display:inline-block;background:var(--error);color:#fff;border-radius:var(--r-sm);padding:0 .4rem;margin-right:var(--s2);font-size:.75em;font-weight:600;vertical-align:middle}
.body{margin-top:var(--s3);border-top:1px solid var(--border);padding-top:var(--s3)}
pre{white-space:pre-wrap;overflow-wrap:anywhere;font:13.5px/1.55 var(--mono);margin:0}
.muted{color:var(--muted)}
.empty{color:var(--muted);margin:var(--s3) 0}
#notice{margin:var(--s2) 0;font-size:.9em}
#notice:empty{display:none}
.wide{overflow-x:auto}
.head{display:flex;align-items:center;gap:var(--s3);padding:var(--s2) 0 var(--s3)}
.who{margin-left:auto;display:flex;align-items:center;gap:var(--s2);min-width:0}
#whoName{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;max-width:12rem}
.toolbar{display:flex;gap:var(--s2) var(--s3);align-items:center;flex-wrap:wrap;margin-bottom:var(--s3)}
.toolbar [role=status],.toolbar .muted{font-size:.85em}
.tabs{position:sticky;top:0;z-index:2;display:flex;gap:var(--s1);overflow-x:auto;scrollbar-width:none;background:var(--bg);border-bottom:1px solid var(--border);margin:0 calc(-1*var(--s4));padding:0 var(--s4)}
.tabs::-webkit-scrollbar{display:none}
.tabs [role=tab]{flex:none;white-space:nowrap;margin:0 0 -1px;padding:.55rem .8rem;border:0;border-bottom:2px solid transparent;border-radius:0;background:none;color:var(--muted);font-weight:500}
.tabs [role=tab]:hover{color:var(--text)}
.tabs [role=tab][aria-selected=true]{border-bottom-color:var(--accent);color:var(--text);font-weight:650}
.tab-count{display:inline-block;min-width:1.3em;margin-left:.35rem;padding:0 .35rem;border-radius:999px;background:var(--error);color:#fff;font-size:.72em;font-weight:650;line-height:1.5;text-align:center;vertical-align:1px}
.tab-count:empty{display:none}
[role=tabpanel]{margin-top:var(--s4)}
[role=tabpanel][hidden]{display:none}
section.card{margin-top:var(--s4)}
table{border-collapse:collapse;width:100%;font-size:.88em;font-variant-numeric:tabular-nums}
#sessionsTbl th:nth-child(3){text-align:right}
th,td{text-align:left;padding:.4rem .6rem;border-bottom:1px solid var(--border);white-space:nowrap;vertical-align:top}
th{font-size:.85em;font-weight:600;color:var(--muted);background:var(--surface-2)}
tbody tr:hover td{background:var(--surface-2)}
td.dir{max-width:14rem;overflow:hidden;text-overflow:ellipsis;direction:rtl;text-align:left}
tr.offline td{color:var(--offline)}
tr.row-error td:first-child{box-shadow:inset 3px 0 var(--error)}
tr.row-blocked td:first-child{box-shadow:inset 3px 0 var(--blocked)}
.st-idle{color:var(--idle)}.st-busy{color:var(--busy);font-weight:600}.st-compacting{color:var(--compacting);font-weight:600}
.st-error{color:var(--error);font-weight:600}.st-blocked{color:var(--blocked);font-weight:600}
.st-offline,.st-unknown{color:var(--offline)}
.qcard{scroll-margin-top:3.5rem}
.qcard.focus{outline:2px solid var(--accent);outline-offset:2px}
.qcard.answered,.qcard.withdrawn{background:var(--surface-2)}
.pill{display:inline-block;border-radius:var(--r-sm);padding:0 .4rem;margin-right:var(--s2);font-size:.75em;font-weight:600;vertical-align:middle;color:var(--accent);background:var(--accent-soft)}
.pill.pending{color:var(--blocked);background:var(--blocked-bg)}.pill.answered{color:var(--idle);background:var(--idle-bg)}.pill.withdrawn{color:var(--offline);background:var(--offline-bg)}
.qitem{border:1px solid var(--border);border-radius:var(--r);padding:var(--s3);margin:var(--s3) 0;min-width:0}
.qitem legend,.qhead{font-weight:600;padding:0 var(--s1)}
.qtext{margin:var(--s1) 0 var(--s2);white-space:pre-wrap;overflow-wrap:anywhere}
.qopt{display:flex;gap:var(--s2);align-items:flex-start;padding:var(--s2);margin:var(--s1) 0;border:1px solid var(--border);border-radius:var(--r-sm);cursor:pointer;overflow-wrap:anywhere}
.qopt:hover{border-color:var(--accent)}
.qopt input{margin:.2rem 0 0;flex:none;width:1.05rem;height:1.05rem}
.qopt>span{min-width:0;flex:1}
.qopt pre{margin-top:var(--s1);padding:var(--s2);background:var(--surface-2);border-radius:var(--r-sm);font-size:12.5px}
.qother{display:flex;flex-direction:column;gap:var(--s1);margin-top:var(--s2);font-size:.9em}
.qother input,.settings-form input:not([type=checkbox]){width:100%}
.qans{margin:var(--s1) 0;overflow-wrap:anywhere}
.msg{margin:var(--s2) 0;font-weight:600}.msg:empty{display:none}.msg.err{color:var(--error)}.msg.ok{color:var(--idle)}
.settings-form{display:grid;gap:var(--s3);max-width:32rem;margin-top:var(--s4);padding-top:var(--s4);border-top:1px solid var(--border)}
.settings-form+.toolbar{margin-top:var(--s6);padding-top:var(--s4);border-top:1px solid var(--border)}
td.revoked{color:var(--error);font-weight:600}
.settings-form label{display:flex;flex-direction:column;gap:var(--s1);font-size:.9em}
.settings-form label.check{flex-direction:row;align-items:center;gap:var(--s2)}
#settingsTbl td.watchers{white-space:normal;overflow-wrap:anywhere}
#settingsTbl td:last-child{text-align:right}
#settingsTbl td:last-child button{margin-left:var(--s1);padding:.2rem .55rem;font-size:.95em}
.viewswitch{display:inline-flex;border:1px solid var(--border-strong);border-radius:var(--r-sm);overflow:hidden;margin:0 0 var(--s3)}
.toolbar .viewswitch{margin:0}
.viewswitch button{margin:0;border:0;border-radius:0;background:var(--surface);color:var(--muted);padding:.35rem .9rem}
.viewswitch button+button{border-left:1px solid var(--border-strong)}
.viewswitch button[aria-pressed=true]{background:var(--accent-soft);color:var(--accent);font-weight:650}
.decfilters{margin-bottom:var(--s3)}
.decfilters a.button{display:inline-block;text-decoration:none}
.dec{border-bottom:1px solid var(--border);padding:var(--s2) 0}
.dec p{margin:.15rem 0;overflow-wrap:anywhere}
.dec p.muted{font-size:.85em}
.metgrid{display:grid;grid-template-columns:repeat(auto-fit,minmax(min(100%,26rem),1fr));gap:var(--s3)}
.mcard,.pcard{border:1px solid var(--border);border-radius:var(--r);padding:var(--s3) var(--s4);min-width:0;background:var(--surface)}
.mcard.full{grid-column:1/-1}
.mcard h3{margin:0 0 var(--s1)}.mcard h4{margin:var(--s4) 0 var(--s1)}
.mcard>p.muted{margin:0 0 var(--s2);font-size:.85em}
.mcard>p.muted:empty{display:none}
.stats{display:grid;grid-template-columns:repeat(auto-fill,minmax(6rem,1fr));gap:var(--s2);margin:var(--s2) 0}
.stat{background:var(--surface-2);border-radius:var(--r-sm);padding:var(--s2) var(--s3)}
.stat b{display:block;font-size:1.2em;font-weight:650;font-variant-numeric:tabular-nums}.stat span{font-size:.78em;color:var(--muted)}
.chart svg{display:block;width:100%;max-width:26rem;height:auto;margin:var(--s2) 0}
.chart .bar{fill:var(--accent)}.chart .axis{stroke:var(--border-strong)}.chart text{font:11px system-ui,sans-serif;fill:var(--muted)}
tr.over td{background:var(--error-bg)}tr.over td:first-child{box-shadow:inset 3px 0 var(--error)}
td.num,th.num{text-align:right}
table.stack td.wrap{white-space:normal;overflow-wrap:anywhere}
.mthreads{margin:var(--s1) 0;padding-left:1.4rem}
.mthreads li{margin:var(--s1) 0}
.mthreads li.empty{list-style:none;margin-left:-1.4rem}
.mthreads button,.plinks button{margin:0;padding:.3rem .5rem;border:1px solid var(--border);border-radius:var(--r-sm);background:var(--surface);color:var(--accent);text-align:left;width:100%;overflow-wrap:anywhere}
.mthreads small,.plinks small{display:block;color:var(--muted)}
.progrid{display:grid;grid-template-columns:repeat(auto-fit,minmax(min(100%,22rem),1fr));gap:var(--s3);align-items:start}
.pcard h3{display:flex;gap:var(--s2);align-items:center;justify-content:space-between;margin:0 0 var(--s2);min-width:0}
.pcard h3 .pname{min-width:0;overflow-wrap:anywhere}
.pcard h4{margin:var(--s3) 0 var(--s1)}
.pcard p{margin:var(--s1) 0;font-size:.92em}
.pbadge{flex:none;border-radius:999px;padding:.05rem .55rem;font-size:.75em;font-weight:650;color:var(--offline);background:var(--offline-bg)}
.pbadge.b-error{color:var(--error);background:var(--error-bg)}.pbadge.b-blocked{color:var(--blocked);background:var(--blocked-bg)}.pbadge.b-busy{color:var(--busy);background:var(--busy-bg)}.pbadge.b-compacting{color:var(--compacting);background:var(--compacting-bg)}.pbadge.b-idle{color:var(--idle);background:var(--idle-bg)}
.pcard.b-error{border-top:3px solid var(--error)}.pcard.b-blocked{border-top:3px solid var(--blocked)}
.plist{list-style:none;margin:0;padding:0}
.plist li{margin:var(--s1) 0;min-width:0;overflow-wrap:anywhere;font-size:.92em}
.pdir{display:block;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;direction:rtl;text-align:left;color:var(--muted);font:12px/1.4 var(--mono)}
.plinks li.over button{border-color:var(--error);background:var(--error-bg);box-shadow:inset 3px 0 var(--error)}
.plinks li.over small{color:var(--error);font-weight:600}
.filters{display:grid;grid-template-columns:repeat(auto-fill,minmax(9.5rem,1fr));gap:var(--s2);align-items:center}
.filters .actions{display:flex;gap:var(--s2);flex-wrap:wrap;align-items:center;grid-column:1/-1}
#conn{font-size:.85em;color:var(--muted)}
.st-online{color:var(--idle);font-weight:600}
@media (max-width:600px){
body{padding:var(--s3) var(--s3) var(--s5);font-size:14.5px}
.tabs{margin:0 calc(-1*var(--s3));padding:0 var(--s3);flex-wrap:wrap;overflow-x:visible;gap:0}
.tabs [role=tab]{padding:.45rem .55rem}
.card{padding:var(--s3)}
.mcard,.pcard{padding:var(--s3)}
.filters{grid-template-columns:1fr 1fr}
.decfilters>*{flex:1 1 100%}
.decfilters a.button,.decfilters button{text-align:center}
td.dir{max-width:11rem}
table.stack thead{display:none}
table.stack tr{display:block;border-bottom:1px solid var(--border);padding:var(--s2) 0}
table.stack td{display:block;border:0;padding:.1rem .2rem;white-space:normal;overflow-wrap:anywhere;text-align:left;max-width:none;direction:ltr}
table.stack td[data-label]::before{content:attr(data-label) " ";color:var(--muted);font-size:.85em;margin-right:var(--s1)}
table.stack td:first-child{font-weight:650}
table.stack tbody tr:hover td{background:none}
#settingsTbl td:last-child{text-align:left;padding-top:var(--s1)}
#settingsTbl td:last-child button{margin:0 var(--s1) 0 0}
}
</style>
</head>
<body>
<header class="head"><h1>` + headerText + `</h1><span class="who"><span id="whoName" class="muted"></span><button type="button" id="logout" hidden>로그아웃</button></span></header>
<nav id="tabs" class="tabs" role="tablist" aria-label="탭">
<button type="button" role="tab" id="tab-progress" data-tab="progress" aria-controls="panel-progress" aria-selected="true" tabindex="0"><span class="tab-name">진행</span><span class="tab-count"></span></button>
<button type="button" role="tab" id="tab-sessions" data-tab="sessions" aria-controls="panel-sessions" aria-selected="false" tabindex="-1"><span class="tab-name">세션</span><span class="tab-count"></span></button>
<button type="button" role="tab" id="tab-metrics" data-tab="metrics" aria-controls="panel-metrics" aria-selected="false" tabindex="-1"><span class="tab-name">지표</span><span class="tab-count"></span></button>
<button type="button" role="tab" id="tab-mail" data-tab="mail" aria-controls="panel-mail" aria-selected="false" tabindex="-1"><span class="tab-name">메일</span><span class="tab-count"></span></button>
<button type="button" role="tab" id="tab-permissions" data-tab="permissions" aria-controls="panel-permissions" aria-selected="false" tabindex="-1"><span class="tab-name">권한 요청</span><span class="tab-count"></span></button>
<button type="button" role="tab" id="tab-questions" data-tab="questions" aria-controls="panel-questions" aria-selected="false" tabindex="-1"><span class="tab-name">질문</span><span class="tab-count"></span></button>
<button type="button" role="tab" id="tab-settings" data-tab="settings" aria-controls="panel-settings" aria-selected="false" tabindex="-1"><span class="tab-name">설정</span><span class="tab-count"></span></button>
</nav>
<section id="panel-progress" role="tabpanel" aria-labelledby="tab-progress" tabindex="0" class="card">
<div class="toolbar"><h2>진행</h2><button id="reloadProgress">새로 고침</button><span id="progressNote" class="muted" role="status" aria-live="polite"></span></div>
<div id="progress" class="progrid"></div>
</section>
<section id="panel-sessions" role="tabpanel" aria-labelledby="tab-sessions" tabindex="0" class="card" hidden>
<div class="toolbar"><h2>세션</h2><button id="reloadSessions">새로 고침</button><span id="sessionsNote" class="muted"></span></div>
<div class="wide"><table id="sessionsTbl" class="stack"><thead><tr><th>역할</th><th>상태</th><th>컨텍스트</th><th>마지막 턴</th><th>기계</th><th>폴더</th><th>라벨</th><th>부모</th><th>마지막 보고</th></tr></thead><tbody id="sessions"></tbody></table></div>
</section>
<section id="panel-metrics" role="tabpanel" aria-labelledby="tab-metrics" tabindex="0" class="card" hidden>
<div class="toolbar"><h2>지표</h2>
<div class="viewswitch" role="group" aria-label="기간 고르기"><button type="button" id="metDays7" data-days="7" aria-pressed="true">7일</button><button type="button" id="metDays30" data-days="30" aria-pressed="false">30일</button></div>
<button id="reloadMetrics">새로 고침</button><span id="metricsNote" class="muted" role="status" aria-live="polite"></span></div>
<div class="metgrid">
<div class="mcard full"><h3 title="스레드를 연 메일이 첫 답을 받기까지 걸린 시간">첫 답장</h3>
<div id="mFirstStats" class="stats"></div>
<div id="mFirstChart" class="chart"></div>
<table id="mFirstTbl" class="stack"><thead><tr><th>받는 쪽</th><th class="num">메일</th><th class="num">답 받음</th><th class="num">답 없음</th><th class="num">p50</th><th class="num">p90</th><th class="num">최장</th></tr></thead><tbody id="mFirstByRecipient"></tbody></table>
</div>
<div class="mcard"><h3 title="역할별 안 읽힌 메일과 가장 오래된 것">안 읽힌 메일</h3>
<p id="mPendingNote" class="muted"></p>
<table id="mPendingTbl" class="stack"><thead><tr><th>역할</th><th class="num">안 읽음</th><th class="num">가장 오래된 것</th></tr></thead><tbody id="mPending"></tbody></table>
</div>
<div class="mcard"><h3 title="두 쪽이 두 번 넘게 주고받은 스레드">되물음</h3>
<p id="mThreadsNote" class="muted"></p>
<ol id="mThreads" class="mthreads"></ol>
</div>
<div class="mcard full"><h3 title="기간 안에 오류·막힘 상태로 있던 시간">세션 상태</h3>
<p id="mSessionsNote" class="muted"></p>
<table id="mStateTbl" class="stack"><thead><tr><th>역할</th><th class="num">오류</th><th class="num">막힘</th><th>많았던 상태</th></tr></thead><tbody id="mStates"></tbody></table>
<h4>지금 오류·막힘</h4>
<table id="mCurrentTbl" class="stack"><thead><tr><th>역할</th><th>상태</th><th>시작</th><th class="num">지난 시간</th></tr></thead><tbody id="mCurrent"></tbody></table>
</div>
</div>
<p id="mAliases" class="muted"></p>
</section>
<section id="panel-mail" role="tabpanel" aria-labelledby="tab-mail" tabindex="0" hidden>
<div class="card filters">
<select id="box" aria-label="메일함"><option value="all">모든 메일</option><option value="inbox">받은 메일</option><option value="outbox">보낸 메일</option><option value="held">보관 중(라벨)</option></select>
<select id="role" aria-label="역할"><option value="">모든 역할</option></select>
<select id="status" aria-label="상태"><option value="">모든 상태</option><option value="pending">안 읽음</option><option value="read">읽음</option><option value="acknowledged">확인함</option><option value="complete">완료</option></select>
<input id="search" aria-label="검색" placeholder="내용·역할·task 검색">
<input id="thread" type="number" min="1" aria-label="스레드 번호" placeholder="스레드 번호">
<input id="task" aria-label="task id" placeholder="task id" autocomplete="off" autocapitalize="off" spellcheck="false">
<div class="actions">
<button id="go" type="submit">검색</button>
<button id="reset">초기화</button>
<button id="reloadMail">새로 고침</button>
<span id="conn">연결 중…</span>
</div>
</div>
<div id="notice" class="muted"></div>
<div id="list"></div>
</section>
<section id="panel-permissions" role="tabpanel" aria-labelledby="tab-permissions" tabindex="0" class="card" hidden>
<div class="toolbar"><h2>권한 요청</h2></div>
<p class="empty">준비 중</p>
</section>
<section id="panel-questions" role="tabpanel" aria-labelledby="tab-questions" tabindex="0" class="card" hidden>
<div class="toolbar"><h2>질문</h2>
<div class="viewswitch" role="group" aria-label="질문 보기 고르기"><button type="button" id="viewQuestions" aria-pressed="true">질문</button><button type="button" id="viewDecisions" aria-pressed="false">결정</button></div>
<button id="reloadQuestions">새로 고침</button><span id="questionsNote" class="muted"></span></div>
<div id="questionsView">
<div id="questionsMsg" class="msg" role="status" aria-live="polite"></div>
<div id="questions"></div>
</div>
<div id="decisionsView" hidden>
<form id="decForm" class="row decfilters">
<select id="decPeriod" aria-label="기간" title="날짜는 UTC 기준"><option value="7">최근 7일</option><option value="30" selected>최근 30일</option><option value="90">최근 90일</option><option value="all">전체</option><option value="custom">직접 고르기</option></select>
<input id="decFrom" type="date" aria-label="시작일" title="UTC 기준" hidden>
<input id="decTo" type="date" aria-label="끝일" title="UTC 기준" hidden>
<input id="decRole" list="decRoles" aria-label="역할" placeholder="역할 또는 svc/*" autocomplete="off" autocapitalize="off" spellcheck="false">
<datalist id="decRoles"></datalist>
<input id="decSearch" type="search" aria-label="검색" placeholder="제목·질문·고른 답 검색">
<button type="submit">검색</button>
<a id="decExport" class="button" download href="/api/admin/decisions?format=ndjson">JSON 내보내기</a>
</form>
<div id="decisionsNote" class="muted" role="status" aria-live="polite"></div>
<div id="decisions"></div>
</div>
</section>
<section id="panel-settings" role="tabpanel" aria-labelledby="tab-settings" tabindex="0" class="card" hidden>
<div class="toolbar"><h2>질문 설정</h2><button id="reloadSettings">새로 고침</button><span id="settingsNote" class="muted"></span></div>
<div id="settingsMsg" class="msg" role="status" aria-live="polite"></div>
<div class="wide"><table id="settingsTbl" class="stack"><thead><tr><th>대상</th><th>질문 가능</th><th>지켜보는 역할</th><th>디스코드</th><th>바뀐 때</th><th></th></tr></thead><tbody id="settingsTable"></tbody></table></div>
<form id="settingsForm" class="settings-form">
<label title="default, 역할(svc/coordinator — svc/coordinator-N 포함), 접두사(svc/*). 정확한 대상 → 가장 긴 접두사 → default 순으로 하나만 적용">대상 <input id="setTarget" required autocomplete="off" autocapitalize="off" spellcheck="false" placeholder="svc/coordinator 또는 svc/*"></label>
<label class="check"><input type="checkbox" id="setCanAsk"> 질문할 수 있음 (can_ask)</label>
<label class="check"><input type="checkbox" id="setDiscord"> 디스코드 알림 (discord)</label>
<label>지켜보는 역할 (쉼표로 구분, 최대 20) <input id="setWatchers" autocomplete="off" autocapitalize="off" spellcheck="false" placeholder="team/boss, ops/lead"></label>
<div class="row"><button type="submit">저장</button><button type="button" id="setClear">비우기</button></div>
</form>
<div class="toolbar"><h2>역할</h2><button id="reloadRoles">새로 고침</button><span id="rolesNote" class="muted"></span></div>
<div id="rolesMsg" class="msg" role="status" aria-live="polite"></div>
<div class="wide"><table id="rolesTbl" class="stack"><thead><tr><th>역할</th><th>등록</th><th>끊은 때</th><th></th></tr></thead><tbody id="rolesTable"></tbody></table></div>
</section>
<script>
const sessionLabel={idle:'유휴',busy:'바쁨',compacting:'compact 중',unknown:'모름',offline:'오프라인'};
function sessionStatus(st){
  const m=/^(error|blocked):(.*)$/.exec(st||'');
  if(m)return [(m[1]==='error'?'오류: ':'막힘: ')+m[2],'st-'+m[1]];
  return [sessionLabel[st]||st,'st-'+st];
}
const statusLabel={pending:'안 읽음',read:'읽음',acknowledged:'확인함',complete:'완료',completed:'완료',held:'보관 중'};
const $=id=>document.getElementById(id);
// adminMode is the mode from /api/admin/me. Outside the proxy-secret
// transition mode a 401 means the session ended: reload to the login page.
let adminMode='';
function signedOut(){if(adminMode&&adminMode!=='legacy')location.reload()}
async function loadMe(){
  try{
    const r=await fetch('/api/admin/me',{credentials:'same-origin',headers:{'Accept':'application/json'}});
    if(!r.ok)return;
    const m=await r.json();adminMode=m.mode||'';
    $('whoName').textContent=m.mode==='legacy'?'프록시':m.user;
    $('whoName').title=$('whoName').textContent;
    $('logout').hidden=m.mode==='legacy';
  }catch(e){}
}
$('logout').onclick=async()=>{
  $('logout').disabled=true;
  try{await fetch('/auth/logout',{method:'POST',credentials:'same-origin',headers:{'Content-Type':'application/json','X-Switchboard-Dashboard':'1'},body:'{}'})}catch(e){}
  location.replace('/');
};
async function api(path){
  let r;
  try{r=await fetch('/api/admin/'+path,{credentials:'same-origin',headers:{'Accept':'application/json'}})}
  catch(e){throw Error('네트워크 오류 · 연결과 로그인 확인')}
  if(r.status===401)signedOut();
  if(r.redirected||!(r.headers.get('content-type')||'').includes('application/json'))throw Error('로그인 필요 · 다시 로그인한 뒤 새로 고침');
  if(!r.ok)throw Error(r.status===401?'로그인 필요(401)':'요청 실패('+r.status+')');
  return r.json();
}
// apiSend makes an admin write with the write guard headers. Errors carry the
// HTTP status (e.status) and the server's message.
async function apiSend(method,path,body){
  let r;
  try{r=await fetch('/api/admin/'+path,{method:method,credentials:'same-origin',headers:{'Accept':'application/json','Content-Type':'application/json','X-Switchboard-Dashboard':'1'},body:body===undefined?undefined:JSON.stringify(body)})}
  catch(e){throw Error('네트워크 오류 · 연결과 로그인 확인')}
  if(r.redirected)throw Error('로그인 필요 · 다시 로그인한 뒤 새로 고침');
  if(r.ok){
    if(!(r.headers.get('content-type')||'').includes('application/json'))throw Error('로그인 필요 · 다시 로그인한 뒤 새로 고침');
    return r.json();
  }
  if(r.status===401)signedOut();
  let text='';try{text=(await r.text()).trim().slice(0,300)}catch(e){}
  const e=Error(r.status===401?'로그인 필요(401)':r.status===403?'쓰기 거부(403) · 같은 주소에서 열었는지 확인':'요청 실패('+r.status+')'+(text?': '+text:''));
  e.status=r.status;throw e;
}
function node(tag,text,cls){const n=document.createElement(tag);n.textContent=text;if(cls)n.className=cls;return n}
async function initMail(){
  try{for(const v of await api('roles'))$('role').append(new Option(v.role,v.role));await load()}
  catch(e){$('conn').textContent=e.message}
}
function kTokens(n){if(n===null||n===undefined)return '-';return n<10000?(n/1000).toFixed(1)+'k':Math.round(n/1000)+'k'}
function when(t){return t?new Date(t).toLocaleString('ko-KR'):'-'}
function byLabel(by){return by==='panel'?'세션 패널':by}
function ago(now,t){if(!t)return '-';const s=Math.max(0,Math.round((new Date(now)-new Date(t))/1000));return s<120?s+'초 전':s<7200?Math.round(s/60)+'분 전':Math.round(s/3600)+'시간 전'}
const sessionCols=['역할','상태','컨텍스트','마지막 턴','기계','폴더','라벨','부모','마지막 보고'];
async function loadSessions(){
  $('sessionsNote').textContent='불러오는 중…';
  try{
    const p=await api('presence');
    const body=$('sessions');body.replaceChildren();
    for(const v of p.sessions){
      const [stText,stClass]=sessionStatus(v.status);
      const tr=node('tr','',!v.online?'offline':/^st-(error|blocked)$/.test(stClass)?'row'+stClass.slice(2):'');
      const cells=[v.role,stText,kTokens(v.context_tokens),when(v.last_turn_at),v.machine,v.directory,v.label||'-',v.parent||'-',ago(p.now,v.reported_at)];
      cells.forEach((text,i)=>{const td=node('td',text,i===1?stClass:i===5?'dir':i===2?'num':'');if(i>0)td.dataset.label=sessionCols[i];tr.append(td)});
      // 긴 폴더는 앞쪽을 줄임표로 줄이고(끝 폴더 이름이 보이게) 전체 경로는 툴팁으로. LRM은 rtl 칸에서 / 순서를 지킨다.
      const dir=tr.children[5];if(v.directory){dir.title=v.directory;dir.textContent='\u200e'+v.directory+'\u200e'}
      body.append(tr);
    }
    if(!p.sessions.length)emptyRow(body,9,'세션 없음');
    $('sessionsNote').textContent=p.sessions.length+'개';
    $('sessionsNote').title='심장박동 '+p.interval_seconds+'초, '+p.missed+'번 놓치면 오프라인';
  }catch(e){$('sessionsNote').textContent=e.message}
}
// mailID is set by openMessage (진행 tab) to show one message; 「초기화」 or a
// new search clears it.
let mailID='';
async function load(){
  $('conn').textContent='불러오는 중…';
  const thread=$('thread').value,task=$('task').value.trim(),id=mailID;
  $('notice').textContent=[id?'메일 #'+id:'',thread?'스레드 #'+thread:'',task?'task '+task:''].filter(Boolean).join(' · ')+(id||thread||task?'만 보는 중':'');
  try{
    const q=new URLSearchParams({box:$('box').value,role:$('role').value,status:$('status').value,q:$('search').value,thread:thread,task:task,id:id});
    const rows=await api('messages?'+q);
    const list=$('list');list.replaceChildren();
    for(const m of rows)list.append(card(m));
    if(!rows.length)list.append(node('p','메일 없음','empty'));
    if(id&&rows.length===1)list.querySelector('article button').click();
    $('conn').textContent=rows.length+'통 (최대 500통)';
  }catch(e){$('conn').textContent=e.message}
}
function card(m){
  const urgent=m.priority==='urgent';
  const c=node('article','',urgent?'card urgent':'card');
  const meta=m.sender+' → '+m.recipient+' · '+new Date(m.created_at).toLocaleString('ko-KR')+' · '+(statusLabel[m.status]||m.status)+(m.task_id?' · task '+m.task_id:'')+(m.addressed_to&&m.addressed_to!==m.recipient?' · '+m.addressed_to+' 앞':'');
  const toggle=node('button','열기');
  toggle.setAttribute('aria-expanded','false');
  let body=null;
  toggle.onclick=()=>{
    if(body){body.remove();body=null;toggle.textContent='열기';toggle.setAttribute('aria-expanded','false');return}
    body=node('div','','body');
    body.append(node('pre',m.body));
    const root=m.thread_id||m.id;
    const foot=node('p','','row muted');
    foot.append(node('span',m.thread_id?'스레드 #'+m.thread_id+(m.reply_to?' · #'+m.reply_to+'에 대한 답장':''):'스레드 시작 메일'));
    const th=node('button','스레드 보기');
    th.onclick=()=>{$('thread').value=root;load();window.scrollTo({top:0})};
    foot.append(th);
    body.append(foot);
    c.append(body);
    toggle.textContent='닫기';toggle.setAttribute('aria-expanded','true');
    body.scrollIntoView({block:'nearest'});
  };
  const h=node('h3','');
  if(urgent)h.append(node('span','긴급','badge'));
  h.append(document.createTextNode('#'+m.id+' '+m.subject));
  c.append(h,node('p',meta,'muted'),toggle);
  return c;
}
// ---- Tabs ----
// Each tab's loader runs once, the first time the tab is shown; after that the
// tab's own 「새로 고침」 button reloads it. #33 sets tabLoaders.permissions and
// #42 sets tabLoaders.questions / tabLoaders.settings (null = nothing to load).
const tabNames=['progress','sessions','metrics','mail','permissions','questions','settings'];
const tabLoaders={progress:null,sessions:loadSessions,metrics:null,mail:initMail,permissions:null,questions:null,settings:null};
const tabLoaded=new Set();
// setTabCount(name,n) shows n as a small count next to the tab name; n
// null/undefined hides it.
// #42 calls it for 'questions' (open questions), #33 for 'permissions' (pending requests),
// #51 for 'metrics' (alert thresholds crossed now; 0 hides the number).
function setTabCount(name,n){
  const t=$('tab-'+name);if(!t)return;
  t.querySelector('.tab-count').textContent=(n===null||n===undefined)?'':String(n);
}
// ---- 지표 ----
// One GET /api/admin/metrics?days=N fills the four cards. metricsSeq drops a
// slow answer for a period the user has already switched away from.
let metricsDays=7,metricsSeq=0;
function dur(s){
  if(s===null||s===undefined)return '-';
  if(s<60)return s+'초';
  if(s<3600)return Math.round(s/60)+'분';
  if(s<86400){const h=s/3600;return (h<10?h.toFixed(1).replace(/\.0$/,''):Math.round(h))+'시간'}
  const d=s/86400;return (d<10?d.toFixed(1).replace(/\.0$/,''):Math.round(d))+'일';
}
const bucketLabel={'<10m':'<10분','<1h':'<1시간','<4h':'<4시간','<1d':'<1일','>=1d':'≥1일'};
function stateText(st){return sessionStatus(st)[0]}
// metricsBreaches counts what is over an alert threshold now: pending mail the
// server flagged, plus current error/blocked sessions past their limit (0 = off).
function metricsBreaches(m){
  const t=m.thresholds||{};
  let n=(m.pending||[]).filter(p=>p.over_threshold).length;
  for(const c of (m.sessions&&m.sessions.current)||[])if(sessionOver(c,t))n++;
  return n;
}
function sessionOver(c,t){
  const lim=/^error:/.test(c.state)?t.error_s:/^blocked:/.test(c.state)?t.blocked_s:0;
  return lim>0&&c.seconds>=lim;
}
function cells(tr,labels,values,classes){
  values.forEach((v,i)=>{const td=node('td',v,(classes&&classes[i])||'');if(i>0)td.dataset.label=labels[i];tr.append(td)});
  return tr;
}
function emptyRow(body,cols,text){const tr=node('tr','');const td=node('td',text,'muted');td.colSpan=cols;tr.append(td);body.append(tr)}
const SVG='http://www.w3.org/2000/svg';
function svgEl(tag,attrs,text){const e=document.createElementNS(SVG,tag);for(const k in attrs)e.setAttribute(k,attrs[k]);if(text!==undefined)e.textContent=text;return e}
// bucketChart draws buckets as bars, each with its count above and label below.
function bucketChart(buckets){
  const W=320,H=140,top=18,bottom=24,gap=10,n=Math.max(buckets.length,1);
  const bw=(W-gap*(n+1))/n,max=Math.max(1,...buckets.map(b=>b.count)),plot=H-top-bottom;
  const svg=svgEl('svg',{viewBox:'0 0 '+W+' '+H,role:'img','aria-label':'첫 답장 분포: '+buckets.map(b=>(bucketLabel[b.label]||b.label)+' '+b.count+'통').join(', ')});
  svg.append(svgEl('title',{},'첫 답장까지 걸린 시간 분포'));
  buckets.forEach((b,i)=>{
    const x=gap+i*(bw+gap),h=Math.round(plot*b.count/max),y=top+plot-h;
    const g=svgEl('g',{});
    g.append(svgEl('rect',{class:'bar',x:x,y:y,width:bw,height:Math.max(h,b.count?1:0)}));
    g.append(svgEl('text',{x:x+bw/2,y:y-4,'text-anchor':'middle'},String(b.count)));
    g.append(svgEl('text',{x:x+bw/2,y:H-8,'text-anchor':'middle'},bucketLabel[b.label]||b.label));
    svg.append(g);
  });
  svg.append(svgEl('line',{class:'axis',x1:0,x2:W,y1:top+plot+.5,y2:top+plot+.5}));
  return svg;
}
function statTile(label,value){const d=node('div','','stat');d.append(node('b',value),node('span',label));return d}
function renderFirstReply(f){
  $('mFirstStats').replaceChildren(statTile('스레드 시작',String(f.count)),statTile('답 받음',String(f.answered)),statTile('답 없음',String(f.unanswered)),statTile('p50',dur(f.p50_s)),statTile('p90',dur(f.p90_s)),statTile('최장',dur(f.max_s)));
  $('mFirstChart').replaceChildren(f.count?bucketChart(f.buckets||[]):node('p','이 기간 스레드 없음','empty'));
  const body=$('mFirstByRecipient');body.replaceChildren();
  const labels=['받는 쪽','메일','답 받음','답 없음','p50','p90','최장'],num=['','num','num','num','num','num','num'];
  for(const r of f.by_recipient||[])body.append(cells(node('tr',''),labels,[r.base,String(r.count),String(r.answered),String(r.unanswered),dur(r.p50_s),dur(r.p90_s),dur(r.max_s)],num));
  if(!body.children.length)emptyRow(body,7,'받는 쪽 없음');
}
function renderPending(list,t){
  $('mPendingNote').textContent=t.pending_s>0?'문턱 '+dur(t.pending_s):'알림 꺼짐';
  const body=$('mPending');body.replaceChildren();
  for(const p of list){
    const tr=cells(node('tr','',p.over_threshold?'over':''),['역할','안 읽음','가장 오래된 것'],[p.role,p.pending+'통',dur(p.oldest_s)+(p.over_threshold?' · 문턱 넘음':'')],['','num','num']);
    body.append(tr);
  }
  if(!list.length)emptyRow(body,3,'안 읽힌 메일 없음');
}
function renderThreads(th){
  $('mThreadsNote').textContent=th.multi_round?'스레드 '+th.multi_round+'개':'';
  const ol=$('mThreads');ol.replaceChildren();
  for(const x of th.top||[]){
    const li=node('li',''),b=node('button','');
    b.type='button';
    b.append(document.createTextNode('#'+x.thread_id+' '+(x.subject||'(제목 없음)')),node('small',(x.parties||[]).join(' ↔ ')+' · 왕복 '+x.round_trips+'번'));
    b.onclick=()=>openThread(x.thread_id);
    li.append(b);ol.append(li);
  }
  if(!ol.children.length)ol.append(node('li','되물은 스레드 없음','empty'));
}
function renderSessionStates(ss,t){
  const lim=[t.error_s>0?'오류 '+dur(t.error_s):'',t.blocked_s>0?'막힘 '+dur(t.blocked_s):''].filter(Boolean);
  $('mSessionsNote').textContent=lim.length?'문턱 '+lim.join(' · '):'알림 꺼짐';
  const body=$('mStates');body.replaceChildren();
  for(const r of ss.by_role||[]){
    const kinds=(r.top_kinds||[]).map(k=>stateText(k.state)+' '+dur(k.seconds)).join(', ')||'-';
    body.append(cells(node('tr',''),['역할','오류','막힘','많았던 상태'],[r.role,dur(r.error_s),dur(r.blocked_s),kinds],['','num','num','wrap']));
  }
  if(!body.children.length)emptyRow(body,4,'오류·막힘 없음');
  const cur=$('mCurrent');cur.replaceChildren();
  for(const c of ss.current||[]){
    const over=sessionOver(c,t),[text,cls]=sessionStatus(c.state);
    cur.append(cells(node('tr','',over?'over':''),['역할','상태','시작','지난 시간'],[c.role,text,when(c.since),dur(c.seconds)+(over?' · 문턱 넘음':'')],['',cls,'','num']));
  }
  if(!cur.children.length)emptyRow(cur,4,'지금 오류·막힘 없음');
}
// renderAliases shows old-name use since the server started (#72).
function renderAliases(a){
  a=a||{};const h=Object.entries(a.old_headers||{}).sort().map(([k,v])=>k+' '+v),e=a.old_env||[];
  $('mAliases').textContent='옛 이름 헤더: '+(h.join(', ')||'0')+' · 옛 이름 환경 변수: '+(e.join(', ')||'없음');
}
async function loadMetrics(){
  const seq=++metricsSeq;
  $('metricsNote').textContent='불러오는 중…';
  try{
    const m=await api('metrics?days='+metricsDays);
    if(seq!==metricsSeq)return;
    const t=m.thresholds||{};
    renderFirstReply(m.first_reply);renderPending(m.pending||[],t);renderThreads(m.threads||{multi_round:0,top:[]});renderSessionStates(m.sessions||{},t);renderAliases(m.aliases);
    const n=metricsBreaches(m);setTabCount('metrics',n||null);
    $('metricsNote').textContent='최근 '+m.window.days+'일 · '+when(m.window.from)+' ~ '+when(m.window.to)+(n?' · 문턱 넘은 것 '+n+'개':'');
  }catch(e){if(seq===metricsSeq)$('metricsNote').textContent=e.message}
}
// refreshMetricsCount fills the tab number before the tab is first opened.
async function refreshMetricsCount(){
  try{const m=await api('metrics?days=1');if(!tabLoaded.has('metrics')){const n=metricsBreaches(m);setTabCount('metrics',n||null)}}catch(e){}
}
function setMetricsDays(d){
  metricsDays=d;
  for(const b of [$('metDays7'),$('metDays30')])b.setAttribute('aria-pressed',Number(b.dataset.days)===d?'true':'false');
  loadMetrics();
}
// openThread shows the 메일 tab filtered to one thread (the 「스레드 보기」 filter),
// clearing the other filters so the whole thread shows.
function openThread(id){openMail({thread:id})}
// openMail shows the 메일 tab with only the given filter (thread, task or id)
// set and every other filter cleared.
function openMail(f){
  $('thread').value=f.thread||'';$('task').value=f.task||'';mailID=f.id?String(f.id):'';
  $('search').value='';$('status').value='';$('role').value='';$('box').value='all';
  const fresh=!tabLoaded.has('mail');
  selectTab('mail');
  if(!fresh)load();
  window.scrollTo({top:0});
}
function openMessage(id){openMail({id:id})}
function openTask(task){openMail({task:task})}
// ---- 진행 ----
// One GET /api/admin/progress fills a card per service. The tab number is the
// open directives older than the pending alert threshold (0 hides it).
const badgeLabel={error:'오류',blocked:'막힘',busy:'바쁨',compacting:'compact 중',idle:'유휴',none:'세션 없음'};
function rel(now,t){if(!t)return '-';const s=Math.max(0,Math.round((new Date(now)-new Date(t))/1000));return s<60?'방금':dur(s)+' 전'}
function linkItem(label,sub,over,onclick){
  const li=node('li','',over?'over':''),b=node('button','');
  b.type='button';b.append(document.createTextNode(label),node('small',sub));b.onclick=onclick;
  li.append(b);return li;
}
function progressCard(p,now,limit){
  const c=node('article','','pcard b-'+p.badge);
  const h=node('h3','');h.append(node('span',p.service,'pname'),node('span',badgeLabel[p.badge]||p.badge,'pbadge b-'+p.badge));
  const news=node('p','');news.append(node('b','마지막 소식 '));
  const r=p.last_report;
  if(r){news.append(document.createTextNode((r.subject||'(제목 없음)')+' · '+rel(now,r.at)));news.title=r.from+' → '+r.to+' · '+when(r.at)}
  else news.append(document.createTextNode(p.last_activity_at?'보고 없음 · 마지막 활동 '+rel(now,p.last_activity_at):'소식 없음'));
  c.append(h,news);
  if(p.sessions.length){
    c.append(node('h4','세션 ('+p.sessions.length+')'));
    const ul=node('ul','','plist');
    for(const v of p.sessions){
      const li=node('li',''),[text,cls]=sessionStatus(v.state);
      li.append(node('b',v.role),document.createTextNode(' · '),node('span',text,cls),node('span',' · '+dur(Math.max(0,Math.round((new Date(now)-new Date(v.since))/1000)))+'째','muted'));
      // 폴더는 #346처럼 앞쪽을 줄임표로 줄이고 전체 경로는 툴팁으로.
      if(v.directory){const d=node('span','\u200e'+v.directory+'\u200e','pdir');d.title=v.directory;li.append(d)}
      ul.append(li);
    }
    c.append(ul);
  }
  if(p.open_directives.length){
    c.append(node('h4','답 없는 지시 ('+p.open_directives.length+')'));
    const ul=node('ul','','plist plinks');
    for(const d of p.open_directives)ul.append(linkItem('#'+d.id+' '+(d.subject||'(제목 없음)'),d.to+' · '+(statusLabel[d.status]||d.status)+' · '+dur(d.age_s)+' 지남'+(d.over_threshold?' · 문턱('+dur(limit)+') 넘음':''),d.over_threshold,()=>openMessage(d.id)));
    c.append(ul);
  }
  if(p.open_tasks.length){
    c.append(node('h4','진행 중 task ('+p.open_tasks.length+')'));
    const ul=node('ul','','plist plinks');
    for(const t of p.open_tasks)ul.append(linkItem(t.task_id,(t.last_subject||'(제목 없음)')+' · '+rel(now,t.last_at)+' · '+t.messages+'통',false,()=>openTask(t.task_id)));
    c.append(ul);
  }
  return c;
}
let progressSeq=0;
async function loadProgress(){
  const seq=++progressSeq;
  $('progressNote').textContent='불러오는 중…';
  try{
    const r=await api('progress');
    if(seq!==progressSeq)return;
    const box=$('progress');box.replaceChildren();
    let over=0;
    for(const p of r.services){over+=p.open_directives.filter(d=>d.over_threshold).length;box.append(progressCard(p,r.generated_at,r.pending_alert_s))}
    if(!r.services.length)box.append(node('p','서비스 없음','empty'));
    setTabCount('progress',over||null);
    $('progressNote').textContent=r.services.length+'개 서비스 · '+when(r.generated_at)+(over?' · 오래된 지시 '+over+'개':'');
  }catch(e){if(seq===progressSeq)$('progressNote').textContent=e.message}
}
tabLoaders.progress=loadProgress;
tabLoaders.metrics=loadMetrics;
// ---- 질문 ----
const qStatusLabel={pending:'대기',answered:'답함',withdrawn:'거둠'};
let questionCards=new Map(),questionsLoaded=false,wantFocus=null,formSeq=0;
function setMsg(el,text,kind){el.textContent=text;el.className='msg'+(kind?' '+kind:'')}
async function refreshQuestionCount(){
  try{setTabCount('questions',(await api('questions?status=pending&limit=500')).questions.length)}catch(e){}
}
async function loadQuestions(){
  $('questionsNote').textContent='불러오는 중…';
  try{
    const list=(await api('questions?limit=200')).questions;
    const box=$('questions');box.replaceChildren();questionCards=new Map();
    for(const q of list){const c=questionCard(q);questionCards.set(q.id,c);box.append(c)}
    const open=list.filter(q=>q.status==='pending').length;
    setTabCount('questions',open);
    $('questionsNote').textContent='대기 '+open+'개 · 모두 '+list.length+'개(최대 200개)';
    if(!list.length)box.append(node('p','질문 없음','empty'));
    questionsLoaded=true;
    if(wantFocus!==null)focusQuestion(wantFocus);
  }catch(e){$('questionsNote').textContent=e.message}
}
function questionCard(q){
  const c=node('article','','card qcard '+q.status);
  c.tabIndex=-1;
  const h=node('h3','');
  h.append(node('span',qStatusLabel[q.status]||q.status,'pill '+q.status));
  if(q.answerer==='owner')h.append(node('span','대표','pill owner'));
  h.append(document.createTextNode(q.questions.map(x=>x.header).join(' · ')));
  c.append(h,node('p',q.asker+' · '+when(q.created_at)+' · '+q.id,'muted'));
  if(q.status==='pending'&&q.can_answer){c.append(answerForm(q));return c}
  q.questions.forEach((item,i)=>{
    const f=node('div','','qitem');
    const head=node('div','','qhead');head.append(node('span',item.header,'pill'));
    f.append(head,node('p',item.question,'qtext'));
    const a=q.answers&&q.answers[i];
    if(a){
      const sel=(a.selected||[]).join(', ');
      f.append(node('p','답: '+(sel||'(선택 없음)'),'qans'));
      if(a.other)f.append(node('p','기타: '+a.other,'qans'));
    }else if(q.status==='pending'){
      for(const o of item.options)f.append(node('p','· '+o.label+(o.description?' — '+o.description:''),'qans muted'));
    }
    c.append(f);
  });
  if(q.status==='answered')c.append(node('p','답한 쪽: '+byLabel(q.answered_by)+' · '+when(q.answered_at),'muted'));
  if(q.status==='withdrawn')c.append(node('p','거둔 때: '+when(q.withdrawn_at),'muted'));
  return c;
}
function answerForm(q){
  const form=node('form','');
  const n=++formSeq;
  const items=q.questions.map((item,i)=>{
    const f=node('fieldset','','qitem');
    const lg=node('legend','');lg.append(node('span',item.header,'pill'),document.createTextNode(item.multiSelect?' 여러 개 고름':' 하나 고름'));
    f.append(lg,node('p',item.question,'qtext'));
    const inputs=[];
    for(const o of item.options){
      const lab=node('label','','qopt');
      const inp=document.createElement('input');
      inp.type=item.multiSelect?'checkbox':'radio';inp.name='q'+n+'-'+i;inp.value=o.label;
      inputs.push(inp);
      const t=node('span','');t.append(node('strong',o.label));
      if(o.description)t.append(document.createTextNode(' — '+o.description));
      if(o.preview)t.append(node('pre',o.preview));
      lab.append(inp,t);f.append(lab);
    }
    const ol=node('label','','qother');
    const other=document.createElement('input');other.type='text';other.maxLength=1000;other.placeholder='고른 것 대신, 또는 덧붙여 쓸 말';
    ol.append(node('span','기타'),other);f.append(ol);
    form.append(f);
    return {item,inputs,other};
  });
  const msg=node('div','','msg');msg.setAttribute('role','status');
  const send=node('button','답 보내기');send.type='submit';
  const row=node('div','','row');row.append(send);
  form.append(row,msg);
  form.onsubmit=async e=>{
    e.preventDefault();
    const answers=items.map(x=>{
      const a={selected:x.inputs.filter(i=>i.checked).map(i=>i.value)};
      const o=x.other.value.trim();if(o)a.other=o;
      return a;
    });
    const missing=items.findIndex((x,i)=>!answers[i].selected.length&&!answers[i].other);
    if(missing>=0){setMsg(msg,'「'+items[missing].item.header+'」에 고르거나 기타를 쓰세요.','err');return}
    send.disabled=true;setMsg(msg,'보내는 중…');
    try{
      await apiSend('POST','questions/'+encodeURIComponent(q.id)+'/answer',{answers:answers});
      setMsg($('questionsMsg'),'답을 보냈습니다: '+q.id,'ok');
    }catch(err){
      send.disabled=false;
      if(err.status!==409){setMsg(msg,err.message,'err');return}
      setMsg($('questionsMsg'),'이미 답했거나 거둔 질문입니다: '+q.id,'err');
    }
    wantFocus=q.id;await loadQuestions();
  };
  return form;
}
// focusQuestion(id) is called for #q=<id> after the 질문 tab is shown. Before
// the list has loaded it remembers id and runs again when it has.
function focusQuestion(id){
  wantFocus=id;
  if(!questionsLoaded)return;
  wantFocus=null;
  for(const c of questionCards.values())c.classList.remove('focus');
  const c=questionCards.get(id);
  if(!c){setMsg($('questionsMsg'),'질문 '+id+' 없음','err');return}
  c.classList.add('focus');
  c.scrollIntoView({block:'start'});
  c.focus({preventScroll:true});
}
tabLoaders.questions=loadQuestions;
// ---- 결정 (#50) ----
// The 결정 view of the 질문 tab lists answered questions as decisions. Its
// filters build one query used both for the list and for the 「JSON 내보내기」
// link (format=ndjson, a same-origin GET the browser downloads).
let decisionsLoaded=false;
function showQuestionView(name){
  const dec=name==='decisions';
  $('viewQuestions').setAttribute('aria-pressed',dec?'false':'true');
  $('viewDecisions').setAttribute('aria-pressed',dec?'true':'false');
  $('questionsView').hidden=dec;$('decisionsView').hidden=!dec;
  if(dec&&!decisionsLoaded){decisionsLoaded=true;loadDecisionRoles();loadDecisions()}
}
function decisionQuery(){
  const p=new URLSearchParams(),period=$('decPeriod').value;
  if(period==='custom'){
    if($('decFrom').value)p.set('from',$('decFrom').value);
    if($('decTo').value)p.set('to',$('decTo').value);
  }else if(period!=='all'){
    p.set('from',new Date(Date.now()-Number(period)*864e5).toISOString().replace(/\.\d+Z$/,'Z'));
  }
  const role=$('decRole').value.trim(),q=$('decSearch').value.trim();
  if(role)p.set('role',role);
  if(q)p.set('q',q);
  return p;
}
function updateDecisionExport(){
  const p=decisionQuery();p.set('format','ndjson');
  $('decExport').href='/api/admin/decisions?'+p.toString();
}
async function loadDecisionRoles(){
  try{
    const roles=(await api('roles')).map(v=>v.role),seen=new Set(),list=$('decRoles');
    const add=v=>{if(!seen.has(v)){seen.add(v);const o=document.createElement('option');o.value=v;list.append(o)}};
    for(const r of roles){const i=r.lastIndexOf('/');if(i>0)add(r.slice(0,i)+'/*')}
    for(const r of roles)add(r);
  }catch(e){}
}
async function loadDecisions(){
  updateDecisionExport();
  $('decisionsNote').textContent='불러오는 중…';
  try{
    const list=(await api('decisions?'+decisionQuery().toString())).decisions;
    const box=$('decisions');box.replaceChildren();
    for(const d of list)box.append(decisionRow(d));
    $('decisionsNote').textContent='결정 '+list.length+'개'+(list.length>=1000?'(최대 1000개)':'');
    if(!list.length)box.append(node('p','조건에 맞는 결정 없음','empty'));
  }catch(e){$('decisionsNote').textContent=e.message}
}
function decisionRow(d){
  const row=node('div','','dec');
  row.append(node('p',d.asker+' · '+when(d.answered_at)+' · '+d.id+' · 답한 쪽 '+byLabel(d.answered_by),'muted'));
  for(const it of d.items){
    const p=node('p','');
    let chosen=(it.chosen||[]).join(', ');
    if(it.other)chosen+=(chosen?' / ':'')+'기타: '+it.other;
    p.title=it.question;
    p.append(node('span',it.header,'pill'),document.createTextNode('→ '+(chosen||'(선택 없음)')));
    row.append(p);
  }
  return row;
}
$('decPeriod').onchange=()=>{const c=$('decPeriod').value==='custom';$('decFrom').hidden=!c;$('decTo').hidden=!c;loadDecisions()};
$('decForm').onsubmit=e=>{e.preventDefault();loadDecisions()};
for(const id of ['decFrom','decTo','decRole','decSearch'])$(id).addEventListener('input',updateDecisionExport);
$('viewQuestions').onclick=()=>selectTab('questions');
$('viewDecisions').onclick=()=>selectTab('decisions');
// ---- 질문 설정 ----
async function loadSettings(){
  $('settingsNote').textContent='불러오는 중…';
  try{
    const rows=await api('question-settings');
    const body=$('settingsTable');body.replaceChildren();
    for(const v of rows){
      const tr=node('tr','');
      const cells=[['대상',v.target],['질문 가능',v.can_ask?'예':'아니오'],['지켜보는 역할',v.watchers.length?v.watchers.join(', '):'-'],['디스코드',v.discord?'켬':'끔'],['바뀐 때',when(v.updated_at)]];
      for(const [label,text] of cells){const td=node('td',text,label==='지켜보는 역할'?'watchers':'');td.dataset.label=label;tr.append(td)}
      const act=node('td','');
      const edit=node('button','편집');edit.type='button';
      edit.onclick=()=>{$('setTarget').value=v.target;$('setCanAsk').checked=v.can_ask;$('setDiscord').checked=v.discord;$('setWatchers').value=v.watchers.join(', ');$('settingsForm').scrollIntoView({block:'nearest'});$('setTarget').focus()};
      act.append(edit);
      if(v.target!=='default'){
        const del=node('button','삭제');del.type='button';
        del.onclick=async()=>{
          if(!confirm(v.target+' 설정을 지울까요?'))return;
          try{await apiSend('DELETE','question-settings/'+targetPath(v.target));setMsg($('settingsMsg'),v.target+' 설정을 지웠습니다.','ok');loadSettings()}
          catch(e){setMsg($('settingsMsg'),e.message,'err')}
        };
        act.append(del);
      }
      tr.append(act);body.append(tr);
    }
    if(!rows.length)emptyRow(body,6,'설정 없음');
    $('settingsNote').textContent=rows.length+'개';
  }catch(e){$('settingsNote').textContent=e.message}
}
// targetPath keeps the slashes of a target (svc/coordinator, svc/*) as path separators.
function targetPath(t){return t.split('/').map(encodeURIComponent).join('/')}
$('settingsForm').onsubmit=async e=>{
  e.preventDefault();
  const target=$('setTarget').value.trim();
  if(!target){setMsg($('settingsMsg'),'대상을 쓰세요.','err');return}
  const watchers=$('setWatchers').value.split(',').map(x=>x.trim()).filter(Boolean);
  try{
    await apiSend('PUT','question-settings/'+targetPath(target),{can_ask:$('setCanAsk').checked,watchers:watchers,discord:$('setDiscord').checked});
    setMsg($('settingsMsg'),target+' 설정을 저장했습니다.','ok');loadSettings();
  }catch(err){setMsg($('settingsMsg'),err.message,'err')}
};
$('setClear').onclick=()=>{$('settingsForm').reset();setMsg($('settingsMsg'),'')};
// ---- 역할 ----
async function loadRoles(){
  $('rolesNote').textContent='불러오는 중…';
  try{
    const rows=await api('roles');
    const body=$('rolesTable');body.replaceChildren();
    for(const v of rows){
      const tr=node('tr','');
      for(const [label,text] of [['역할',v.role],['등록',when(v.created_at)],['끊은 때',v.revoked_at?when(v.revoked_at):'-']]){const td=node('td',text,label==='끊은 때'&&v.revoked_at?'revoked':'');td.dataset.label=label;tr.append(td)}
      const act=node('td','');
      const revoke=!v.revoked_at;
      const b=node('button',revoke?'끊기':'되살리기');b.type='button';
      b.onclick=async()=>{
        if(!confirm(revoke?v.role+' 역할을 끊을까요? 이 토큰으로 접속한 세션이 모두 끊깁니다.':v.role+' 역할을 되살릴까요?'))return;
        try{await apiSend('POST','roles/'+targetPath(v.role)+(revoke?'/revoke':'/restore'));setMsg($('rolesMsg'),v.role+(revoke?' 역할을 끊었습니다.':' 역할을 되살렸습니다.'),'ok');loadRoles()}
        catch(e){setMsg($('rolesMsg'),e.message,'err')}
      };
      act.append(b);tr.append(act);body.append(tr);
    }
    if(!rows.length)emptyRow(body,4,'역할 없음');
    const off=rows.filter(v=>v.revoked_at).length;
    $('rolesNote').textContent=rows.length+'개'+(off?' · 끊김 '+off:'');
  }catch(e){$('rolesNote').textContent=e.message}
}
$('reloadRoles').onclick=loadRoles;
tabLoaders.settings=()=>{loadSettings();loadRoles()};
function showTab(name){
  for(const n of tabNames){
    const on=n===name,t=$('tab-'+n);
    t.setAttribute('aria-selected',on?'true':'false');
    t.tabIndex=on?0:-1;
    $('panel-'+n).hidden=!on;
  }
  if(!tabLoaded.has(name)){tabLoaded.add(name);if(tabLoaders[name])tabLoaders[name]()}
}
// Routes: #progress #sessions #metrics #mail #permissions #questions #settings, #decisions (질문 tab,
// 결정 view) and #q=<id> (질문 + focusQuestion). No hash or an unknown one shows 진행.
function route(){
  const h=location.hash.slice(1);
  if(h==='decisions'){showTab('questions');showQuestionView('decisions');return}
  if(h==='questions'||h.startsWith('q='))showQuestionView('questions');
  if(h.startsWith('q=')){
    showTab('questions');
    let id=h.slice(2);try{id=decodeURIComponent(id)}catch(e){}
    focusQuestion(id);
    return;
  }
  showTab(tabNames.includes(h)?h:'progress');
}
// selectTab(name) also takes 'decisions' (the 결정 view of the 질문 tab).
function selectTab(name){
  if(location.hash==='#'+name)route();else location.hash=name;
}
for(const n of tabNames)$('tab-'+n).onclick=()=>selectTab(n);
$('tabs').addEventListener('keydown',e=>{
  const i=tabNames.findIndex(n=>$('tab-'+n)===document.activeElement);
  if(i<0)return;
  let j;
  if(e.key==='ArrowRight')j=(i+1)%tabNames.length;
  else if(e.key==='ArrowLeft')j=(i-1+tabNames.length)%tabNames.length;
  else if(e.key==='Home')j=0;
  else if(e.key==='End')j=tabNames.length-1;
  else return;
  e.preventDefault();
  selectTab(tabNames[j]);
  $('tab-'+tabNames[j]).focus();
});
window.addEventListener('hashchange',route);

$('go').onclick=()=>{mailID='';load()};
$('reloadMail').onclick=load;
$('reloadSessions').onclick=loadSessions;
$('reloadProgress').onclick=loadProgress;
$('reloadMetrics').onclick=loadMetrics;
$('metDays7').onclick=()=>setMetricsDays(7);
$('metDays30').onclick=()=>setMetricsDays(30);
$('reloadQuestions').onclick=()=>{setMsg($('questionsMsg'),'');loadQuestions()};
$('reloadSettings').onclick=loadSettings;
$('reset').onclick=()=>{$('thread').value='';$('task').value='';mailID='';$('search').value='';$('status').value='';$('role').value='';$('box').value='all';load()};
$('search').addEventListener('keydown',e=>{if(e.key==='Enter')load()});
$('thread').addEventListener('keydown',e=>{if(e.key==='Enter')load()});
$('task').addEventListener('keydown',e=>{if(e.key==='Enter'){mailID='';load()}});
loadMe();
route();
if(!tabLoaded.has('questions'))refreshQuestionCount();
if(!tabLoaded.has('metrics'))refreshMetricsCount();
</script>
</body>
</html>
`
