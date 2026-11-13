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
.body{margin-top:.8rem;border-top:1px solid #dde;padding-top:.8rem}
pre{white-space:pre-wrap;overflow-wrap:anywhere;font:15px/1.5 ui-monospace,monospace;margin:0}
.muted{color:#667}
#notice{margin:.4rem 0}
</style>
</head>
<body>
<h1>Agent Mail · 관리자 보기</h1>
<p class="muted">등록된 모든 역할과 메일함을 읽기 전용으로 봅니다.</p>
<section class="card row">
<select id="box"><option value="all">모든 메일</option><option value="inbox">받은 메일</option><option value="outbox">보낸 메일</option></select>
<select id="role"><option value="">모든 역할</option></select>
<select id="status"><option value="">모든 상태</option><option value="pending">안 읽음</option><option value="read">읽음</option><option value="acknowledged">확인함</option><option value="complete">완료</option></select>
<input id="search" placeholder="내용·역할·task 검색">
<input id="thread" type="number" min="1" placeholder="스레드 번호">
<button id="go">검색</button>
<button id="reset">초기화</button>
<span id="conn">연결 중…</span>
</section>
<div id="notice" class="muted"></div>
<div id="list"></div>
<script>
const statusLabel={pending:'안 읽음',read:'읽음',acknowledged:'확인함',complete:'완료',completed:'완료'};
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
async function init(){
  try{for(const role of await api('roles'))$('role').append(new Option(role,role));await load()}
  catch(e){$('conn').textContent=e.message}
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
  const c=node('article','','card');
  const meta=m.sender+' → '+m.recipient+' · '+new Date(m.created_at).toLocaleString('ko-KR')+' · '+(statusLabel[m.status]||m.status)+(m.task_id?' · task '+m.task_id:'');
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
  c.append(node('h3','#'+m.id+' '+m.subject),node('p',meta,'muted'),toggle);
  return c;
}
$('go').onclick=load;
$('reset').onclick=()=>{$('thread').value='';$('search').value='';$('status').value='';$('role').value='';$('box').value='all';load()};
$('search').addEventListener('keydown',e=>{if(e.key==='Enter')load()});
$('thread').addEventListener('keydown',e=>{if(e.key==='Enter')load()});
init();
</script>
</body>
</html>
`
