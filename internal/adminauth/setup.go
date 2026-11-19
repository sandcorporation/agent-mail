package adminauth

// First-run admin setup of a hosting PC instance (#221). Only `up` opens it
// (server.Up passes its data directory; `serve` never does), only in the
// builtin mode, and only while admin_users is empty:
//
//   - a one-time setup code is written to <data>/setup-code (user only) and
//     printed once in the log;
//   - every request must reach the loopback listener from a loopback peer
//     with a loopback Host, and carry no forwarding header;
//   - five wrong codes in a row replace the code (the file gets the new one);
//   - the admin is created only after one TOTP code from the new secret is
//     confirmed, and the first admin (from this page or `admin create`)
//     removes the code file and turns every setup path into 404.

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sandcorporation/switchboard/internal/config"
	"rsc.io/qr"
)

const (
	SetupCodeFile = "setup-code"
	setupPath     = "/setup"
	setupFailMax  = 5 // wrong codes in a row before the code is replaced
	// setupPendingTTL bounds the time between the QR code and its confirmation.
	setupPendingTTL = 10 * time.Minute
)

// setupCodeEncoding is base32 without the letters people misread.
var setupCodeEncoding = base32.NewEncoding("ABCDEFGHJKLMNPQRSTUVWXYZ23456789").WithPadding(base32.NoPadding)

// Setup is the open first-run setup of one instance. A nil *Setup is a
// closed one.
type Setup struct {
	path  string                        // <data>/setup-code
	write func(path, code string) error // creates path readable by this user only

	mu      sync.Mutex
	code    string // "" once closed
	fails   int
	pending *pendingAdmin
}

// pendingAdmin is a checked name and password waiting for its TOTP
// confirmation.
type pendingAdmin struct {
	name, hash, secret string
	expires            time.Time
	wrong              int // wrong TOTP codes; setupFailMax drops the pending admin
}

// OpenSetup opens the setup page when db has no admin user yet: it writes a
// new code to path through write and returns it, for the caller to print
// once. With an admin present it removes a stale code file and returns nil.
func OpenSetup(db *sql.DB, path string, write func(path, code string) error) (*Setup, string, error) {
	has, err := hasAdmin(db)
	if err != nil {
		return nil, "", err
	}
	if has {
		if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, "", err
		}
		return nil, "", nil
	}
	st := &Setup{path: path, write: write}
	if err = st.newCode(); err != nil {
		return nil, "", err
	}
	return st, st.code, nil
}

func hasAdmin(db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM admin_users`).Scan(&n)
	return n > 0, err
}

// newCode replaces the code and its file. Called with mu held (or before
// the Setup is shared).
func (st *Setup) newCode() error {
	raw := make([]byte, 10)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	c := setupCodeEncoding.EncodeToString(raw) // 16 characters
	code := c[:4] + "-" + c[4:8] + "-" + c[8:12] + "-" + c[12:]
	if err := os.Remove(st.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := st.write(st.path, code); err != nil {
		return err
	}
	st.code, st.fails, st.pending = code, 0, nil
	return nil
}

// close ends the setup for good. Called with mu held.
func (st *Setup) close() {
	st.code, st.pending = "", nil
	if err := os.Remove(st.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("admin setup: cannot remove %s: %v", st.path, err)
	}
}

func normalizeSetupCode(c string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '-' || r == ' ':
			return -1
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		}
		return r
	}, c)
}

// checkCode compares a submitted code. Called with mu held.
func (st *Setup) checkCode(submitted string) bool {
	want, got := normalizeSetupCode(st.code), normalizeSetupCode(submitted)
	if len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
		st.fails = 0
		return true
	}
	st.fails++
	if st.fails >= setupFailMax {
		if err := st.newCode(); err != nil {
			log.Printf("admin setup: cannot replace the code: %v", err)
			st.close()
			return false
		}
		log.Printf("admin setup: %d wrong codes in a row; a new code is in %s", setupFailMax, st.path)
	}
	return false
}

// loopbackRequest reports whether r came to a loopback listener from a
// loopback peer, named a loopback host and passed no proxy.
func loopbackRequest(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" {
		return false
	}
	isLoopback := func(hostport string) bool {
		host, _, err := net.SplitHostPort(hostport)
		if err != nil {
			host = strings.Trim(hostport, "[]")
		}
		if host == "localhost" {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	return ok && isLoopback(local.String()) && isLoopback(r.RemoteAddr) && isLoopback(r.Host)
}

type setupRequest struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Password string `json:"password"`
	TOTP     string `json:"totp"`
}

// SetupBegun is the answer to POST /setup/begin: the new TOTP secret, its
// otpauth URI and the URI's QR code (rows of '1' dark and '0' light modules,
// without the quiet zone) for the page to draw.
type SetupBegun struct {
	Name       string   `json:"name"`
	Secret     string   `json:"secret"`
	OtpauthURI string   `json:"otpauth_uri"`
	QR         []string `json:"qr"`
}

// setupError answers with status and a reason the page shows its own text
// for.
func setupError(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, map[string]string{"error": reason})
}

// SetupAPI serves GET /setup (the page), POST /setup/begin and POST
// /setup/confirm over st.
func (s *Service) SetupAPI(w http.ResponseWriter, r *http.Request, st *Setup) {
	w.Header().Set("Cache-Control", "no-store")
	if st == nil || s.cfg.Mode != ModeBuiltin {
		http.NotFound(w, r)
		return
	}
	if !loopbackRequest(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.code == "" {
		http.NotFound(w, r)
		return
	}
	if has, err := hasAdmin(s.db); err != nil || has {
		if err == nil {
			st.close()
		}
		http.NotFound(w, r)
		return
	}
	switch {
	case r.URL.Path == setupPath && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, setupHTML)
		return
	case r.URL.Path == setupPath+"/begin" || r.URL.Path == setupPath+"/confirm":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	if !s.writeAllowed(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req setupRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		setupError(w, http.StatusBadRequest, "request")
		return
	}
	if !st.checkCode(req.Code) {
		setupError(w, http.StatusForbidden, "code")
		return
	}
	if r.URL.Path == setupPath+"/begin" {
		s.setupBegin(w, st, req)
		return
	}
	s.setupConfirm(w, r, st, req)
}

func (s *Service) setupBegin(w http.ResponseWriter, st *Setup, req setupRequest) {
	if !adminNamePattern.MatchString(req.Name) {
		setupError(w, http.StatusBadRequest, "name")
		return
	}
	if checkNewPassword(req.Password) != nil {
		setupError(w, http.StatusBadRequest, "password")
		return
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		http.Error(w, "setup failed", http.StatusInternalServerError)
		return
	}
	secret, err := newTOTPSecret()
	if err != nil {
		http.Error(w, "setup failed", http.StatusInternalServerError)
		return
	}
	uri := otpauthURI(req.Name, secret)
	rows, err := qrRows(uri)
	if err != nil {
		http.Error(w, "setup failed", http.StatusInternalServerError)
		return
	}
	st.pending = &pendingAdmin{name: req.Name, hash: hash, secret: secret, expires: s.now().Add(setupPendingTTL)}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, SetupBegun{Name: req.Name, Secret: secret, OtpauthURI: uri, QR: rows})
}

func (s *Service) setupConfirm(w http.ResponseWriter, r *http.Request, st *Setup, req setupRequest) {
	now := s.now()
	p := st.pending
	if p == nil || !now.Before(p.expires) {
		st.pending = nil
		setupError(w, http.StatusConflict, "expired")
		return
	}
	step := matchTOTP(p.secret, req.TOTP, now, 0)
	if step == 0 {
		if p.wrong++; p.wrong >= setupFailMax {
			st.pending = nil
			setupError(w, http.StatusConflict, "expired")
			return
		}
		setupError(w, http.StatusBadRequest, "totp")
		return
	}
	// The first admin only: a concurrent `admin create` wins and closes setup.
	res, err := s.db.Exec(`INSERT INTO admin_users(name,password_hash,totp_secret,totp_last_step,created_at)
SELECT ?,?,?,?,? WHERE NOT EXISTS (SELECT 1 FROM admin_users)`, p.name, p.hash, p.secret, step, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		http.Error(w, "setup failed", http.StatusInternalServerError)
		return
	}
	st.close()
	if n, _ := res.RowsAffected(); n != 1 {
		http.NotFound(w, r)
		return
	}
	log.Printf("admin setup: created admin %s; setup closed", p.name)
	if err = s.startSession(w, r, p.name, ModeBuiltin); err != nil {
		http.Error(w, "session failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	writeJSON(w, Identity{User: p.name, Mode: ModeBuiltin})
}

// qrRows encodes text as a QR code (level M) in rows of '1' and '0'.
func qrRows(text string) ([]string, error) {
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return nil, err
	}
	rows := make([]string, c.Size)
	line := make([]byte, c.Size)
	for y := range c.Size {
		for x := range c.Size {
			line[x] = '0'
			if c.Black(x, y) {
				line[x] = '1'
			}
		}
		rows[y] = string(line)
	}
	return rows, nil
}

// setupHTML is the setup page. It loads nothing from outside the page and
// puts every value into the page as text.
const setupHTML = `<!doctype html>
<html lang="ko">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>` + config.ProductName + ` 관리자 만들기</title>
<style>
:root{color-scheme:light dark;--bg:#f4f6f9;--surface:#fff;--border:#dfe4ec;--border-strong:#c5cdd9;--text:#18202e;--muted:#5f6b7c;--accent:#2457c5;--accent-fg:#fff;--error:#c0302b;--error-bg:#fceaea;--r:8px;--r-sm:5px}
@media (prefers-color-scheme:dark){:root{--bg:#0f1319;--surface:#171c24;--border:#2b3340;--border-strong:#3b4555;--text:#e4e8ef;--muted:#97a1b0;--accent:#7aa5ff;--accent-fg:#0f1319;--error:#ff7b74;--error-bg:#3a1c1c}}
*,*::before,*::after{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:flex-start;justify-content:center;padding:8vh 1rem 2rem;background:var(--bg);color:var(--text);font:15px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,"Noto Sans KR","Apple SD Gothic Neo","Malgun Gothic",sans-serif}
form{width:100%;max-width:22rem;background:var(--surface);border:1px solid var(--border);border-radius:var(--r);padding:1.5rem;display:flex;flex-direction:column;gap:.9rem}
form[hidden]{display:none}
h1{font-size:1.2rem;font-weight:650;margin:0 0 .25rem}
label{display:flex;flex-direction:column;gap:.3rem;font-size:.9rem;color:var(--muted)}
input{font:inherit;color:var(--text);padding:.5rem .6rem;border:1px solid var(--border-strong);border-radius:var(--r-sm);background:var(--surface);width:100%;min-width:0}
button{font:inherit;padding:.55rem .75rem;border:1px solid var(--accent);border-radius:var(--r-sm);background:var(--accent);color:var(--accent-fg);cursor:pointer}
button:disabled{opacity:.6;cursor:default}
:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
.err{margin:0;padding:.5rem .6rem;border-radius:var(--r-sm);background:var(--error-bg);color:var(--error);font-size:.9rem}
.err:empty{display:none}
#qr{display:block;width:100%;max-width:15rem;height:auto;margin:0 auto;border-radius:var(--r-sm)}
details{font-size:.85rem;color:var(--muted)}
summary{cursor:pointer}
#secret{display:block;margin-top:.4rem;font:13px/1.4 ui-monospace,SFMono-Regular,Consolas,monospace;color:var(--text);word-break:break-all;user-select:all}
</style>
</head>
<body>
<form id="begin" autocomplete="off">
<h1>관리자 만들기</h1>
<label>설치 코드<input id="code" name="setup-code" autocomplete="off" autocapitalize="characters" spellcheck="false" required maxlength="40"></label>
<label>이름<input id="name" name="username" autocomplete="username" required maxlength="64" pattern="[A-Za-z0-9][A-Za-z0-9._@\-]{0,63}" autocapitalize="none" spellcheck="false"></label>
<label>비밀번호 (12자 이상)<input id="password" name="new-password" type="password" autocomplete="new-password" required minlength="12"></label>
<label>비밀번호 확인<input id="password2" type="password" autocomplete="new-password" required minlength="12"></label>
<p id="err1" class="err" role="alert"></p>
<button type="submit" id="next">다음</button>
</form>
<form id="confirm" hidden autocomplete="off">
<h1>인증 앱 등록</h1>
<svg id="qr" role="img" aria-label="TOTP QR"></svg>
<details><summary>QR을 읽을 수 없을 때</summary><code id="secret"></code></details>
<label>인증 코드<input id="totp" name="code" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9]{6}" maxlength="6" required></label>
<p id="err2" class="err" role="alert"></p>
<button type="submit" id="done">관리자 만들기</button>
</form>
<script>
(function(){
const $=function(id){return document.getElementById(id)};
const texts={code:'설치 코드가 맞지 않습니다',name:'이름은 영문·숫자·. _ @ - 로 64자까지입니다',password:'비밀번호는 12자 이상이어야 합니다',totp:'인증 코드가 맞지 않습니다',expired:'시간이 지났습니다. 처음부터 다시 하세요',request:'요청이 올바르지 않습니다'};
async function post(path,body){
  const r=await fetch(path,{method:'POST',credentials:'same-origin',headers:{'Content-Type':'application/json','` + config.HeaderPrefix + `Dashboard':'1'},body:JSON.stringify(body)});
  let j=null;try{j=await r.json()}catch(e){}
  return {ok:r.ok,status:r.status,body:j};
}
function fail(el,res){
  if(res.status===404){location.replace('/');return}
  el.textContent=(res.body&&texts[res.body.error])||'만들지 못했습니다';
}
function drawQR(rows){
  const svg=$('qr'),ns='http://www.w3.org/2000/svg',n=rows.length,q=4;
  svg.setAttribute('viewBox','0 0 '+(n+2*q)+' '+(n+2*q));
  svg.setAttribute('shape-rendering','crispEdges');
  const bg=document.createElementNS(ns,'rect');
  bg.setAttribute('width',n+2*q);bg.setAttribute('height',n+2*q);bg.setAttribute('fill','#fff');
  svg.appendChild(bg);
  let d='';
  for(let y=0;y<n;y++)for(let x=0;x<n;x++)if(rows[y].charAt(x)==='1')d+='M'+(x+q)+' '+(y+q)+'h1v1h-1z';
  const p=document.createElementNS(ns,'path');
  p.setAttribute('d',d);p.setAttribute('fill','#000');
  svg.appendChild(p);
}
let code='';
$('begin').addEventListener('submit',async function(ev){
  ev.preventDefault();const err=$('err1'),btn=$('next');err.textContent='';
  if($('password').value!==$('password2').value){err.textContent='비밀번호가 서로 다릅니다';return}
  btn.disabled=true;code=$('code').value.trim();
  try{
    const res=await post('/setup/begin',{code:code,name:$('name').value,password:$('password').value});
    if(res.ok&&res.body&&Array.isArray(res.body.qr)){
      drawQR(res.body.qr);$('secret').textContent=res.body.secret;
      $('begin').hidden=true;$('confirm').hidden=false;$('totp').focus();return;
    }
    fail(err,res);
  }catch(e){err.textContent='네트워크 오류'}
  btn.disabled=false;
});
$('confirm').addEventListener('submit',async function(ev){
  ev.preventDefault();const err=$('err2'),btn=$('done');err.textContent='';btn.disabled=true;
  try{
    const res=await post('/setup/confirm',{code:code,totp:$('totp').value});
    if(res.ok){location.replace('/');return}
    if(res.body&&res.body.error==='expired'){$('confirm').hidden=true;$('begin').hidden=false;$('next').disabled=false}
    fail(res.body&&res.body.error==='expired'?$('err1'):err,res);
  }catch(e){err.textContent='네트워크 오류'}
  $('totp').value='';btn.disabled=false;
});
})();
</script>
</body>
</html>
`
