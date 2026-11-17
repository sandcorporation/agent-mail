package main

import (
	"io"
	"net/http"
)

// dashboard serves the read-only admin overview. It only calls same-origin
// /api/admin/* endpoints, sends no Authorization header and stores nothing in
// the browser. Message fields are rendered as text nodes, never as HTML.
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
.card{background:#fff;padding:1rem;margin:.7rem 0;border-radius:8px;box-shadow:0 1px 5px #ccd}
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
td.dir{white-space:normal;overflow-wrap:anywhere;min-width:10rem}
tr.offline td{color:#889}
.st-busy{color:#b26a00;font-weight:600}.st-compacting{color:#6a1b9a;font-weight:600}.st-idle{color:#2e7d32}
.st-error{color:#c62828;font-weight:600}.st-blocked{color:#b26a00;font-weight:700}
</style>
</head>
<body>
<h1>Agent Mail · 관리자 보기</h1>
<p class="muted">등록된 모든 역할과 메일함을 읽기 전용으로 봅니다.</p>
<nav id="tabs" class="tabs" role="tablist" aria-label="관리자 보기 탭">
<button type="button" role="tab" id="tab-sessions" data-tab="sessions" aria-controls="panel-sessions" aria-selected="true" tabindex="0"><span class="tab-name">세션</span><span class="tab-count"></span></button>
<button type="button" role="tab" id="tab-mail" data-tab="mail" aria-controls="panel-mail" aria-selected="false" tabindex="-1"><span class="tab-name">메일</span><span class="tab-count"></span></button>
<button type="button" role="tab" id="tab-permissions" data-tab="permissions" aria-controls="panel-permissions" aria-selected="false" tabindex="-1"><span class="tab-name">권한 요청</span><span class="tab-count"></span></button>
<button type="button" role="tab" id="tab-questions" data-tab="questions" aria-controls="panel-questions" aria-selected="false" tabindex="-1"><span class="tab-name">질문</span><span class="tab-count"></span></button>
<button type="button" role="tab" id="tab-settings" data-tab="settings" aria-controls="panel-settings" aria-selected="false" tabindex="-1"><span class="tab-name">설정</span><span class="tab-count"></span></button>
</nav>
<section id="panel-sessions" role="tabpanel" aria-labelledby="tab-sessions" tabindex="0" class="card">
<div class="row"><h2 style="margin:0">세션</h2><button id="reloadSessions">새로 고침</button><span id="sessionsNote" class="muted"></span></div>
<div class="wide"><table><thead><tr><th>역할</th><th>상태</th><th>컨텍스트</th><th>마지막 턴</th><th>기계</th><th>폴더</th><th>라벨</th><th>부모</th><th>마지막 보고</th></tr></thead><tbody id="sessions"></tbody></table></div>
</section>
<section id="panel-mail" role="tabpanel" aria-labelledby="tab-mail" tabindex="0" hidden>
<div class="card row">
<select id="box"><option value="all">모든 메일</option><option value="inbox">받은 메일</option><option value="outbox">보낸 메일</option><option value="held">보관 중(라벨)</option></select>
<select id="role"><option value="">모든 역할</option></select>
<select id="status"><option value="">모든 상태</option><option value="pending">안 읽음</option><option value="read">읽음</option><option value="acknowledged">확인함</option><option value="complete">완료</option></select>
<input id="search" placeholder="내용·역할·task 검색">
<input id="thread" type="number" min="1" placeholder="스레드 번호">
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
<!-- #42 fills this panel: open questions (loader in tabLoaders.questions, count via setTabCount('questions',n), #q=<id> via focusQuestion). -->
<section id="panel-questions" role="tabpanel" aria-labelledby="tab-questions" tabindex="0" class="card" hidden>
<h2 style="margin:0">질문</h2>
<p class="muted">준비 중</p>
</section>
<!-- #42 fills this panel: per-role question settings (loader in tabLoaders.settings). -->
<section id="panel-settings" role="tabpanel" aria-labelledby="tab-settings" tabindex="0" class="card" hidden>
<h2 style="margin:0">설정</h2>
<p class="muted">준비 중 · 역할별 질문 설정</p>
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
      body.append(tr);
    }
    $('sessionsNote').textContent=p.sessions.length+'개 · 심장박동 '+p.interval_seconds+'초, '+p.missed+'번 놓치면 오프라인';
  }catch(e){$('sessionsNote').textContent=e.message}
}
async function load(){
  $('conn').textContent='불러오는 중…';
  const thread=$('thread').value;
  $('notice').textContent=thread?'스레드 #'+thread+'의 메일만 보는 중입니다. 「초기화」로 돌아갑니다.':'';
  try{
    const q=new URLSearchParams({box:$('box').value,role:$('role').value,status:$('status').value,q:$('search').value,thread:thread});
    const rows=await api('messages?'+q);
    const list=$('list');list.replaceChildren();
    for(const m of rows)list.append(card(m));
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
const tabNames=['sessions','mail','permissions','questions','settings'];
const tabLoaders={sessions:loadSessions,mail:initMail,permissions:null,questions:null,settings:null};
const tabLoaded=new Set();
// setTabCount(name,n) shows 「이름 (n)」 on a tab; n null/undefined hides the number.
// #42 calls it for 'questions' (open questions), #33 for 'permissions' (pending requests).
function setTabCount(name,n){
  const t=$('tab-'+name);if(!t)return;
  t.querySelector('.tab-count').textContent=(n===null||n===undefined)?'':' ('+n+')';
}
// focusQuestion(id) is called for #q=<id> after the 질문 tab is shown.
// Stub: #42 scrolls to and highlights question <id>.
function focusQuestion(id){}
function showTab(name){
  for(const n of tabNames){
    const on=n===name,t=$('tab-'+n);
    t.setAttribute('aria-selected',on?'true':'false');
    t.tabIndex=on?0:-1;
    $('panel-'+n).hidden=!on;
  }
  if(!tabLoaded.has(name)){tabLoaded.add(name);if(tabLoaders[name])tabLoaders[name]()}
}
// Routes: #sessions #mail #permissions #questions #settings, and #q=<id> (질문 + focusQuestion).
// No hash or an unknown one shows 세션.
function route(){
  const h=location.hash.slice(1);
  if(h.startsWith('q=')){
    showTab('questions');
    let id=h.slice(2);try{id=decodeURIComponent(id)}catch(e){}
    focusQuestion(id);
    return;
  }
  showTab(tabNames.includes(h)?h:'sessions');
}
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

$('go').onclick=load;
$('reloadMail').onclick=load;
$('reloadSessions').onclick=loadSessions;
$('reset').onclick=()=>{$('thread').value='';$('search').value='';$('status').value='';$('role').value='';$('box').value='all';load()};
$('search').addEventListener('keydown',e=>{if(e.key==='Enter')load()});
$('thread').addEventListener('keydown',e=>{if(e.key==='Enter')load()});
route();
</script>
</body>
</html>
`
