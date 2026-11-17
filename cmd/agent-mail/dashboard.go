package main

import (
	"io"
	"net/http"
)

// dashboard serves the admin overview. It only calls same-origin /api/admin/*
// endpoints, sends no Authorization header and stores nothing in the browser.
// Its only writes are question answers and question settings, sent with the
// admin write guard headers. Message and question fields are rendered as text
// nodes, never as HTML.
func dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, dashboardHTML)
}

const dashboardHTML = `<!doctype html>
<html lang="ko">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Agent Mail</title>
<style>
body{font:16px system-ui,sans-serif;max-width:1100px;margin:2rem auto;padding:0 1rem;background:#f5f7fa;color:#172033}
input,button,select{font:inherit;padding:.5rem;margin:.2rem}
button{cursor:pointer}
.row{display:flex;gap:.4rem;align-items:center;flex-wrap:wrap}
.card{background:#fff;padding:1rem;margin:.7rem 0;border-radius:8px;box-shadow:0 1px 5px #ccd;overflow-wrap:anywhere}
.card h3{margin:.1rem 0 .3rem}
.card.urgent{border-left:4px solid #c62828}
.badge{display:inline-block;background:#c62828;color:#fff;border-radius:4px;padding:0 .4rem;margin-right:.4rem;font-size:.8em;vertical-align:middle}
.body{margin-top:.8rem;border-top:1px solid #dde;padding-top:.8rem}
pre{white-space:pre-wrap;overflow-wrap:anywhere;font:15px/1.5 ui-monospace,monospace;margin:0}
.muted{color:#667}
#notice{margin:.4rem 0}
.wide{overflow-x:auto}
.tabs{display:flex;gap:.2rem;overflow-x:auto;border-bottom:2px solid #ccd;margin:1rem 0 .3rem}
.tabs [role=tab]{flex:none;white-space:nowrap;margin:0;padding:.6rem .9rem;border:0;border-bottom:3px solid transparent;background:none;color:#445}
.tabs [role=tab][aria-selected=true]{border-bottom-color:#1e5bb8;color:#172033;font-weight:600}
[role=tabpanel][hidden]{display:none}
table{border-collapse:collapse;width:100%;font-size:.9em}
th,td{text-align:left;padding:.3rem .5rem;border-bottom:1px solid #dde;white-space:nowrap}
td.dir{max-width:18rem;overflow:hidden;text-overflow:ellipsis;direction:rtl;text-align:left}
tr.offline td{color:#889}
.st-busy{color:#b26a00;font-weight:600}.st-compacting{color:#6a1b9a;font-weight:600}.st-idle{color:#2e7d32}
.st-error{color:#c62828;font-weight:600}.st-blocked{color:#b26a00;font-weight:700}
.qcard{scroll-margin-top:1rem}
.qcard.focus{outline:3px solid #1e5bb8;outline-offset:2px}
.qcard.answered,.qcard.withdrawn{background:#fafbfd}
.pill{display:inline-block;border-radius:4px;padding:0 .4rem;margin-right:.4rem;font-size:.8em;vertical-align:middle;color:#fff;background:#667}
.pill.pending{background:#b26a00}.pill.answered{background:#2e7d32}.pill.withdrawn{background:#889}
.qitem{border:1px solid #dde;border-radius:6px;padding:.6rem;margin:.6rem 0;min-width:0}
.qitem legend,.qhead{font-weight:600;padding:0 .3rem}
.qhead .pill{background:#1e5bb8}
.qtext{margin:.2rem 0 .4rem;white-space:pre-wrap;overflow-wrap:anywhere}
.qopt{display:flex;gap:.5rem;align-items:flex-start;padding:.4rem;margin:.3rem 0;border:1px solid #e3e7ee;border-radius:6px;cursor:pointer;overflow-wrap:anywhere}
.qopt input{margin:.25rem 0 0;flex:none;width:1.1rem;height:1.1rem}
.qopt>span{min-width:0;flex:1}
.qopt pre{margin-top:.3rem;padding:.4rem;background:#f1f3f7;border-radius:4px;font-size:13px}
.qother{display:flex;flex-direction:column;gap:.2rem;margin-top:.3rem}
.qother input,.settings-form input:not([type=checkbox]){width:100%;box-sizing:border-box;margin:0}
.qans{margin:.2rem 0;overflow-wrap:anywhere}
.msg{margin:.4rem 0;font-weight:600}.msg.err{color:#c62828}.msg.ok{color:#2e7d32}
.settings-form{display:grid;gap:.5rem;max-width:32rem;margin-top:1rem;padding-top:.8rem;border-top:1px solid #dde}
.settings-form label{display:flex;flex-direction:column;gap:.2rem}
.settings-form label.check{flex-direction:row;align-items:center;gap:.4rem}
#settingsTbl td.watchers{white-space:normal;overflow-wrap:anywhere}
.viewswitch{display:inline-flex;border:1px solid #1e5bb8;border-radius:6px;overflow:hidden;margin:.5rem 0}
.viewswitch button{margin:0;border:0;border-radius:0;background:#fff;color:#1e5bb8;padding:.45rem 1rem}
.viewswitch button[aria-pressed=true]{background:#1e5bb8;color:#fff;font-weight:600}
.decfilters input,.decfilters select{min-width:0}
.decfilters a.button{display:inline-block;padding:.5rem .8rem;margin:.2rem;border:1px solid #1e5bb8;border-radius:4px;color:#1e5bb8;text-decoration:none;background:#fff}
.dec{border-bottom:1px solid #e3e7ee;padding:.5rem 0}
.dec p{margin:.15rem 0;overflow-wrap:anywhere}
.dec .pill{background:#1e5bb8}
.metgrid{display:grid;grid-template-columns:repeat(auto-fit,minmax(min(100%,26rem),1fr));gap:.8rem;margin-top:.6rem}
.mcard{border:1px solid #dde;border-radius:6px;padding:.7rem;min-width:0}
.mcard.full{grid-column:1/-1}
.mcard h3{margin:0 0 .3rem}.mcard h4{margin:.8rem 0 .2rem;font-size:.95em}
.mcard p{margin:.2rem 0 .5rem}
.stats{display:grid;grid-template-columns:repeat(auto-fill,minmax(6rem,1fr));gap:.4rem;margin:.4rem 0}
.stat{background:#f1f3f7;border-radius:6px;padding:.4rem .5rem}
.stat b{display:block;font-size:1.25em}.stat span{font-size:.8em;color:#556}
.chart svg{display:block;width:100%;max-width:26rem;height:auto;margin:.4rem 0}
.chart .bar{fill:#1e5bb8}.chart .axis{stroke:#99a}.chart text{font:11px system-ui,sans-serif;fill:#334}
tr.over td{background:#fdecea}tr.over td:first-child{box-shadow:inset 4px 0 #c62828}
td.num,th.num{text-align:right}
table.stack td.wrap{white-space:normal;overflow-wrap:anywhere}
.mthreads{margin:.2rem 0;padding-left:1.4rem}
.mthreads li{margin:.3rem 0}
.mthreads button{margin:0;padding:.3rem .4rem;border:1px solid #c9d3e3;border-radius:4px;background:#fff;color:#1e5bb8;text-align:left;width:100%;overflow-wrap:anywhere}
.mthreads small{display:block;color:#556}
.progrid{display:grid;grid-template-columns:repeat(auto-fit,minmax(min(100%,22rem),1fr));gap:.8rem;margin-top:.6rem;align-items:start}
.pcard{border:1px solid #dde;border-radius:6px;padding:.7rem;min-width:0}
.pcard h3{display:flex;gap:.5rem;align-items:center;justify-content:space-between;margin:0 0 .4rem;min-width:0}
.pcard h3 .pname{min-width:0;overflow-wrap:anywhere}
.pcard h4{margin:.7rem 0 .2rem;font-size:.95em}
.pcard p{margin:.2rem 0}
.pbadge{flex:none;border-radius:4px;padding:.05rem .45rem;font-size:.8em;color:#fff;background:#889}
.pbadge.b-error{background:#c62828}.pbadge.b-blocked{background:#b26a00}.pbadge.b-busy{background:#1e5bb8}.pbadge.b-idle{background:#2e7d32}
.plist{list-style:none;margin:0;padding:0}
.plist li{margin:.3rem 0;min-width:0;overflow-wrap:anywhere}
.pdir{display:block;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;direction:rtl;text-align:left;color:#556;font-size:.85em}
.plinks button{margin:0;padding:.3rem .4rem;border:1px solid #c9d3e3;border-radius:4px;background:#fff;color:#1e5bb8;text-align:left;width:100%;overflow-wrap:anywhere}
.plinks small{display:block;color:#556}
.plinks li.over button{border-color:#c62828;background:#fdecea;box-shadow:inset 4px 0 #c62828}
.plinks li.over small{color:#8e1b1b;font-weight:600}
@media (max-width:600px){
body{margin:1rem auto}
.decfilters>*{flex:1 1 100%;box-sizing:border-box}
.decfilters a.button,.decfilters button{text-align:center}
td.dir{max-width:11rem}
table.stack thead{display:none}
table.stack tr{display:block;border-bottom:1px solid #dde;padding:.3rem 0}
table.stack td{display:block;border:0;padding:.1rem .2rem;white-space:normal;overflow-wrap:anywhere;text-align:left}
table.stack td[data-label]::before{content:attr(data-label) ": ";color:#667}
table.stack td:first-child{font-weight:600}
#settingsTbl thead{display:none}
#settingsTbl tr{display:block;border-bottom:1px solid #dde;padding:.4rem 0}
#settingsTbl td{display:block;border:0;padding:.15rem .2rem;white-space:normal;overflow-wrap:anywhere}
#settingsTbl td[data-label]::before{content:attr(data-label) ": ";color:#667}
}
</style>
</head>
<body>
<h1>Agent Mail · 관리자 보기</h1>
<p class="muted">등록된 모든 역할과 메일함을 봅니다. 바꿀 수 있는 것은 질문 답과 질문 설정뿐입니다.</p>
<nav id="tabs" class="tabs" role="tablist" aria-label="관리자 보기 탭">
<button type="button" role="tab" id="tab-progress" data-tab="progress" aria-controls="panel-progress" aria-selected="true" tabindex="0"><span class="tab-name">진행</span><span class="tab-count"></span></button>
<button type="button" role="tab" id="tab-sessions" data-tab="sessions" aria-controls="panel-sessions" aria-selected="false" tabindex="-1"><span class="tab-name">세션</span><span class="tab-count"></span></button>
<button type="button" role="tab" id="tab-metrics" data-tab="metrics" aria-controls="panel-metrics" aria-selected="false" tabindex="-1"><span class="tab-name">지표</span><span class="tab-count"></span></button>
<button type="button" role="tab" id="tab-mail" data-tab="mail" aria-controls="panel-mail" aria-selected="false" tabindex="-1"><span class="tab-name">메일</span><span class="tab-count"></span></button>
<button type="button" role="tab" id="tab-permissions" data-tab="permissions" aria-controls="panel-permissions" aria-selected="false" tabindex="-1"><span class="tab-name">권한 요청</span><span class="tab-count"></span></button>
<button type="button" role="tab" id="tab-questions" data-tab="questions" aria-controls="panel-questions" aria-selected="false" tabindex="-1"><span class="tab-name">질문</span><span class="tab-count"></span></button>
<button type="button" role="tab" id="tab-settings" data-tab="settings" aria-controls="panel-settings" aria-selected="false" tabindex="-1"><span class="tab-name">설정</span><span class="tab-count"></span></button>
</nav>
<section id="panel-progress" role="tabpanel" aria-labelledby="tab-progress" tabindex="0" class="card">
<div class="row"><h2 style="margin:0">진행</h2><button id="reloadProgress">새로 고침</button><span id="progressNote" class="muted" role="status" aria-live="polite"></span></div>
<p class="muted">서비스마다 지금 하는 일과 마지막 소식입니다. 최근에 움직인 서비스가 먼저입니다. 지시나 task를 누르면 메일 탭에서 엽니다.</p>
<div id="progress" class="progrid"></div>
</section>
<section id="panel-sessions" role="tabpanel" aria-labelledby="tab-sessions" tabindex="0" class="card" hidden>
<div class="row"><h2 style="margin:0">세션</h2><button id="reloadSessions">새로 고침</button><span id="sessionsNote" class="muted"></span></div>
<div class="wide"><table><thead><tr><th>역할</th><th>상태</th><th>컨텍스트</th><th>마지막 턴</th><th>기계</th><th>폴더</th><th>라벨</th><th>부모</th><th>마지막 보고</th></tr></thead><tbody id="sessions"></tbody></table></div>
</section>
<section id="panel-metrics" role="tabpanel" aria-labelledby="tab-metrics" tabindex="0" class="card" hidden>
<div class="row"><h2 style="margin:0">지표</h2>
<div class="viewswitch" role="group" aria-label="기간 고르기"><button type="button" id="metDays7" data-days="7" aria-pressed="true">7일</button><button type="button" id="metDays30" data-days="30" aria-pressed="false">30일</button></div>
<button id="reloadMetrics">새로 고침</button><span id="metricsNote" class="muted" role="status" aria-live="polite"></span></div>
<div class="metgrid">
<div class="mcard full"><h3>첫 답장</h3>
<p class="muted">스레드를 연 메일이 받는 쪽에서 첫 답을 받기까지 걸린 시간입니다.</p>
<div id="mFirstStats" class="stats"></div>
<div id="mFirstChart" class="chart"></div>
<table id="mFirstTbl" class="stack"><thead><tr><th>받는 쪽</th><th>메일</th><th>답 받음</th><th>답 없음</th><th>p50</th><th>p90</th><th>최장</th></tr></thead><tbody id="mFirstByRecipient"></tbody></table>
</div>
<div class="mcard"><h3>안 읽힌 메일</h3>
<p id="mPendingNote" class="muted"></p>
<table id="mPendingTbl" class="stack"><thead><tr><th>역할</th><th>안 읽음</th><th>가장 오래된 것</th></tr></thead><tbody id="mPending"></tbody></table>
</div>
<div class="mcard"><h3>되물음</h3>
<p id="mThreadsNote" class="muted"></p>
<ol id="mThreads" class="mthreads"></ol>
</div>
<div class="mcard full"><h3>세션 상태</h3>
<p id="mSessionsNote" class="muted"></p>
<table id="mStateTbl" class="stack"><thead><tr><th>역할</th><th>오류</th><th>막힘</th><th>많았던 상태</th></tr></thead><tbody id="mStates"></tbody></table>
<h4>지금 오류·막힘</h4>
<table id="mCurrentTbl" class="stack"><thead><tr><th>역할</th><th>상태</th><th>시작</th><th>지난 시간</th></tr></thead><tbody id="mCurrent"></tbody></table>
</div>
</div>
</section>
<section id="panel-mail" role="tabpanel" aria-labelledby="tab-mail" tabindex="0" hidden>
<div class="card row">
<select id="box"><option value="all">모든 메일</option><option value="inbox">받은 메일</option><option value="outbox">보낸 메일</option><option value="held">보관 중(라벨)</option></select>
<select id="role"><option value="">모든 역할</option></select>
<select id="status"><option value="">모든 상태</option><option value="pending">안 읽음</option><option value="read">읽음</option><option value="acknowledged">확인함</option><option value="complete">완료</option></select>
<input id="search" placeholder="내용·역할·task 검색">
<input id="thread" type="number" min="1" placeholder="스레드 번호">
<input id="task" placeholder="task id" autocomplete="off" autocapitalize="off" spellcheck="false">
<button id="go">검색</button>
<button id="reset">초기화</button>
<button id="reloadMail">새로 고침</button>
<span id="conn">연결 중…</span>
</div>
<div id="notice" class="muted"></div>
<div id="list"></div>
</section>
<!-- #33 fills this panel: pending permission requests (loader in tabLoaders.permissions, count via setTabCount('permissions',n)). -->
<section id="panel-permissions" role="tabpanel" aria-labelledby="tab-permissions" tabindex="0" class="card" hidden>
<h2 style="margin:0">권한 요청</h2>
<p class="muted">준비 중</p>
</section>
<section id="panel-questions" role="tabpanel" aria-labelledby="tab-questions" tabindex="0" class="card" hidden>
<div class="row"><h2 style="margin:0">질문</h2><button id="reloadQuestions">새로 고침</button><span id="questionsNote" class="muted"></span></div>
<div class="viewswitch" role="group" aria-label="질문 보기 고르기"><button type="button" id="viewQuestions" aria-pressed="true">질문</button><button type="button" id="viewDecisions" aria-pressed="false">결정</button></div>
<div id="questionsView">
<p class="muted">기다리는 질문이 먼저, 그다음 최근에 답했거나 거둔 질문이 보입니다.</p>
<div id="questionsMsg" class="msg" role="status" aria-live="polite"></div>
<div id="questions"></div>
</div>
<div id="decisionsView" hidden>
<p class="muted">답한 질문을 결정으로 봅니다. 최근에 답한 것이 먼저, 최대 1000개입니다. 날짜는 UTC 기준입니다.</p>
<form id="decForm" class="row decfilters">
<select id="decPeriod" aria-label="기간"><option value="7">최근 7일</option><option value="30" selected>최근 30일</option><option value="90">최근 90일</option><option value="all">전체</option><option value="custom">직접 고르기</option></select>
<input id="decFrom" type="date" aria-label="시작일" hidden>
<input id="decTo" type="date" aria-label="끝일" hidden>
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
<div class="row"><h2 style="margin:0">질문 설정</h2><button id="reloadSettings">새로 고침</button><span id="settingsNote" class="muted"></span></div>
<p class="muted">대상은 default, 역할(svc/coordinator — svc/coordinator-N도 포함), 접두사(svc/*)입니다. 역할마다 정확한 대상, 가장 긴 접두사, default 순으로 하나가 적용됩니다.</p>
<div id="settingsMsg" class="msg" role="status" aria-live="polite"></div>
<div class="wide"><table id="settingsTbl"><thead><tr><th>대상</th><th>질문 가능</th><th>지켜보는 역할</th><th>디스코드</th><th>바뀐 때</th><th></th></tr></thead><tbody id="settingsTable"></tbody></table></div>
<form id="settingsForm" class="settings-form">
<label>대상 <input id="setTarget" required autocomplete="off" autocapitalize="off" spellcheck="false" placeholder="svc/coordinator 또는 svc/*"></label>
<label class="check"><input type="checkbox" id="setCanAsk"> 질문할 수 있음 (can_ask)</label>
<label class="check"><input type="checkbox" id="setDiscord"> 디스코드 알림 (discord)</label>
<label>지켜보는 역할 (쉼표로 구분, 최대 20) <input id="setWatchers" autocomplete="off" autocapitalize="off" spellcheck="false" placeholder="team/boss, ops/lead"></label>
<div class="row"><button type="submit">저장</button><button type="button" id="setClear">비우기</button></div>
</form>
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
async function api(path){
  let r;
  try{r=await fetch('/api/admin/'+path,{credentials:'same-origin',headers:{'Accept':'application/json'}})}
  catch(e){throw Error('네트워크 오류입니다. 사설 연결과 로그인을 확인하세요.')}
  if(r.redirected||!(r.headers.get('content-type')||'').includes('application/json'))throw Error('로그인 페이지로 넘어갔습니다. 보호된 주소로 로그인한 뒤 새로 고치세요.');
  if(!r.ok)throw Error(r.status===401?'이 프록시에 관리자 접근이 설정되지 않았습니다.':'요청 실패('+r.status+')');
  return r.json();
}
// apiSend makes an admin write with the write guard headers. Errors carry the
// HTTP status (e.status) and the server's message.
async function apiSend(method,path,body){
  let r;
  try{r=await fetch('/api/admin/'+path,{method:method,credentials:'same-origin',headers:{'Accept':'application/json','Content-Type':'application/json','X-Agent-Mail-Dashboard':'1'},body:body===undefined?undefined:JSON.stringify(body)})}
  catch(e){throw Error('네트워크 오류입니다. 사설 연결과 로그인을 확인하세요.')}
  if(r.redirected)throw Error('로그인 페이지로 넘어갔습니다. 보호된 주소로 로그인한 뒤 새로 고치세요.');
  if(r.ok){
    if(!(r.headers.get('content-type')||'').includes('application/json'))throw Error('로그인 페이지로 넘어갔습니다. 보호된 주소로 로그인한 뒤 새로 고치세요.');
    return r.json();
  }
  let text='';try{text=(await r.text()).trim().slice(0,300)}catch(e){}
  const e=Error(r.status===401?'이 프록시에 관리자 접근이 설정되지 않았습니다.':r.status===403?'쓰기가 거부되었습니다(403). 같은 주소에서 연 대시보드인지 확인하세요.':'요청 실패('+r.status+')'+(text?': '+text:''));
  e.status=r.status;throw e;
}
function node(tag,text,cls){const n=document.createElement(tag);n.textContent=text;if(cls)n.className=cls;return n}
async function initMail(){
  try{for(const role of await api('roles'))$('role').append(new Option(role,role));await load()}
  catch(e){$('conn').textContent=e.message}
}
function kTokens(n){if(n===null||n===undefined)return '-';return n<10000?(n/1000).toFixed(1)+'k':Math.round(n/1000)+'k'}
function when(t){return t?new Date(t).toLocaleString('ko-KR'):'-'}
function ago(now,t){if(!t)return '-';const s=Math.max(0,Math.round((new Date(now)-new Date(t))/1000));return s<120?s+'초 전':s<7200?Math.round(s/60)+'분 전':Math.round(s/3600)+'시간 전'}
async function loadSessions(){
  $('sessionsNote').textContent='불러오는 중…';
  try{
    const p=await api('presence');
    const body=$('sessions');body.replaceChildren();
    for(const v of p.sessions){
      const tr=node('tr','',v.online?'':'offline');
      const [stText,stClass]=sessionStatus(v.status);
      const cells=[v.role,stText,kTokens(v.context_tokens),when(v.last_turn_at),v.machine,v.directory,v.label||'-',v.parent||'-',ago(p.now,v.reported_at)];
      cells.forEach((text,i)=>tr.append(node('td',text,i===1?stClass:(i===5?'dir':''))));
      // 긴 폴더는 앞쪽을 줄임표로 줄이고(끝 폴더 이름이 보이게) 전체 경로는 툴팁으로. LRM은 rtl 칸에서 / 순서를 지킨다.
      const dir=tr.children[5];if(v.directory){dir.title=v.directory;dir.textContent='\u200e'+v.directory+'\u200e'}
      body.append(tr);
    }
    $('sessionsNote').textContent=p.sessions.length+'개 · 심장박동 '+p.interval_seconds+'초, '+p.missed+'번 놓치면 오프라인';
  }catch(e){$('sessionsNote').textContent=e.message}
}
// mailID is set by openMessage (진행 tab) to show one message; 「초기화」 or a
// new search clears it.
let mailID='';
async function load(){
  $('conn').textContent='불러오는 중…';
  const thread=$('thread').value,task=$('task').value.trim(),id=mailID;
  $('notice').textContent=[id?'메일 #'+id+'만 보는 중입니다.':'',thread?'스레드 #'+thread+'의 메일만 보는 중입니다.':'',task?'task '+task+'의 메일만 보는 중입니다.':''].filter(Boolean).join(' ')+(id||thread||task?' 「초기화」로 돌아갑니다.':'');
  try{
    const q=new URLSearchParams({box:$('box').value,role:$('role').value,status:$('status').value,q:$('search').value,thread:thread,task:task,id:id});
    const rows=await api('messages?'+q);
    const list=$('list');list.replaceChildren();
    for(const m of rows)list.append(card(m));
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
// setTabCount(name,n) shows 「이름 (n)」 on a tab; n null/undefined hides the number.
// #42 calls it for 'questions' (open questions), #33 for 'permissions' (pending requests),
// #51 for 'metrics' (alert thresholds crossed now; 0 hides the number).
function setTabCount(name,n){
  const t=$('tab-'+name);if(!t)return;
  t.querySelector('.tab-count').textContent=(n===null||n===undefined)?'':' ('+n+')';
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
  $('mFirstChart').replaceChildren(f.count?bucketChart(f.buckets||[]):node('p','이 기간에 스레드를 연 메일이 없습니다.','muted'));
  const body=$('mFirstByRecipient');body.replaceChildren();
  const labels=['받는 쪽','메일','답 받음','답 없음','p50','p90','최장'],num=['','num','num','num','num','num','num'];
  for(const r of f.by_recipient||[])body.append(cells(node('tr',''),labels,[r.base,String(r.count),String(r.answered),String(r.unanswered),dur(r.p50_s),dur(r.p90_s),dur(r.max_s)],num));
  if(!body.children.length)emptyRow(body,7,'받는 쪽이 없습니다.');
}
function renderPending(list,t){
  $('mPendingNote').textContent=t.pending_s>0?'가장 오래된 메일이 '+dur(t.pending_s)+' 넘게 안 읽힌 역할을 강조합니다.':'안 읽힌 메일 알림이 꺼져 있습니다.';
  const body=$('mPending');body.replaceChildren();
  for(const p of list){
    const tr=cells(node('tr','',p.over_threshold?'over':''),['역할','안 읽음','가장 오래된 것'],[p.role,p.pending+'통',dur(p.oldest_s)+(p.over_threshold?' · 문턱 넘음':'')],['','num','num']);
    body.append(tr);
  }
  if(!list.length)emptyRow(body,3,'안 읽힌 메일이 없습니다.');
}
function renderThreads(th){
  $('mThreadsNote').textContent='두 쪽이 두 번 넘게 주고받은 스레드 '+th.multi_round+'개'+(th.top&&th.top.length?' · 많은 순으로 '+th.top.length+'개, 누르면 메일 탭에서 그 스레드를 엽니다.':'');
  const ol=$('mThreads');ol.replaceChildren();
  for(const x of th.top||[]){
    const li=node('li',''),b=node('button','');
    b.type='button';
    b.append(document.createTextNode('#'+x.thread_id+' '+(x.subject||'(제목 없음)')),node('small',(x.parties||[]).join(' ↔ ')+' · 왕복 '+x.round_trips+'번'));
    b.onclick=()=>openThread(x.thread_id);
    li.append(b);ol.append(li);
  }
}
function renderSessionStates(ss,t){
  const lim=[t.error_s>0?'오류 '+dur(t.error_s):'',t.blocked_s>0?'막힘 '+dur(t.blocked_s):''].filter(Boolean);
  $('mSessionsNote').textContent='기간 안에 오류·막힘 상태로 있던 시간입니다.'+(lim.length?' 지금 상태가 '+lim.join(', ')+'을 넘으면 강조합니다.':' 세션 알림이 꺼져 있습니다.');
  const body=$('mStates');body.replaceChildren();
  for(const r of ss.by_role||[]){
    const kinds=(r.top_kinds||[]).map(k=>stateText(k.state)+' '+dur(k.seconds)).join(', ')||'-';
    body.append(cells(node('tr',''),['역할','오류','막힘','많았던 상태'],[r.role,dur(r.error_s),dur(r.blocked_s),kinds],['','num','num','wrap']));
  }
  if(!body.children.length)emptyRow(body,4,'기간 안에 오류·막힘이 없었습니다.');
  const cur=$('mCurrent');cur.replaceChildren();
  for(const c of ss.current||[]){
    const over=sessionOver(c,t),[text,cls]=sessionStatus(c.state);
    cur.append(cells(node('tr','',over?'over':''),['역할','상태','시작','지난 시간'],[c.role,text,when(c.since),dur(c.seconds)+(over?' · 문턱 넘음':'')],['',cls,'','num']));
  }
  if(!cur.children.length)emptyRow(cur,4,'지금 오류·막힘인 세션이 없습니다.');
}
async function loadMetrics(){
  const seq=++metricsSeq;
  $('metricsNote').textContent='불러오는 중…';
  try{
    const m=await api('metrics?days='+metricsDays);
    if(seq!==metricsSeq)return;
    const t=m.thresholds||{};
    renderFirstReply(m.first_reply);renderPending(m.pending||[],t);renderThreads(m.threads||{multi_round:0,top:[]});renderSessionStates(m.sessions||{},t);
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
const badgeLabel={error:'오류',blocked:'막힘',busy:'바쁨',idle:'유휴',none:'세션 없음'};
function rel(now,t){if(!t)return '-';const s=Math.max(0,Math.round((new Date(now)-new Date(t))/1000));return s<60?'방금':dur(s)+' 전'}
function linkItem(label,sub,over,onclick){
  const li=node('li','',over?'over':''),b=node('button','');
  b.type='button';b.append(document.createTextNode(label),node('small',sub));b.onclick=onclick;
  li.append(b);return li;
}
function progressCard(p,now,limit){
  const c=node('article','','pcard');
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
    if(!r.services.length)box.append(node('p','아직 서비스가 없습니다.','muted'));
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
    if(!list.length)box.append(node('p','질문이 없습니다.','muted'));
    questionsLoaded=true;
    if(wantFocus!==null)focusQuestion(wantFocus);
  }catch(e){$('questionsNote').textContent=e.message}
}
function questionCard(q){
  const c=node('article','','card qcard '+q.status);
  c.tabIndex=-1;
  const h=node('h3','');
  h.append(node('span',qStatusLabel[q.status]||q.status,'pill '+q.status),document.createTextNode(q.questions.map(x=>x.header).join(' · ')));
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
  if(q.status==='answered')c.append(node('p','답한 쪽: '+q.answered_by+' · '+when(q.answered_at),'muted'));
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
  if(!c){setMsg($('questionsMsg'),'질문 '+id+'을(를) 목록에서 찾지 못했습니다.','err');return}
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
    const roles=await api('roles'),seen=new Set(),list=$('decRoles');
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
    if(!list.length)box.append(node('p','조건에 맞는 결정이 없습니다.','muted'));
  }catch(e){$('decisionsNote').textContent=e.message}
}
function decisionRow(d){
  const row=node('div','','dec');
  row.append(node('p',d.asker+' · '+when(d.answered_at)+' · '+d.id+' · 답한 쪽 '+d.answered_by,'muted'));
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
tabLoaders.settings=loadSettings;
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
route();
if(!tabLoaded.has('questions'))refreshQuestionCount();
if(!tabLoaded.has('metrics'))refreshMetricsCount();
</script>
</body>
</html>
`
