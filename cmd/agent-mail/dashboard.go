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
@media (max-width:600px){
body{margin:1rem auto}
td.dir{max-width:11rem}
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
<section id="panel-questions" role="tabpanel" aria-labelledby="tab-questions" tabindex="0" class="card" hidden>
<div class="row"><h2 style="margin:0">질문</h2><button id="reloadQuestions">새로 고침</button><span id="questionsNote" class="muted"></span></div>
<p class="muted">기다리는 질문이 먼저, 그다음 최근에 답했거나 거둔 질문이 보입니다.</p>
<div id="questionsMsg" class="msg" role="status" aria-live="polite"></div>
<div id="questions"></div>
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
$('reloadQuestions').onclick=()=>{setMsg($('questionsMsg'),'');loadQuestions()};
$('reloadSettings').onclick=loadSettings;
$('reset').onclick=()=>{$('thread').value='';$('search').value='';$('status').value='';$('role').value='';$('box').value='all';load()};
$('search').addEventListener('keydown',e=>{if(e.key==='Enter')load()});
$('thread').addEventListener('keydown',e=>{if(e.key==='Enter')load()});
route();
if(!tabLoaded.has('questions'))refreshQuestionCount();
</script>
</body>
</html>
`
