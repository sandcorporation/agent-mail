package server

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sandcorporation/switchboard/internal/adminauth"
	"github.com/sandcorporation/switchboard/internal/node/fakeagent"
	"github.com/sandcorporation/switchboard/internal/term/tmux"
)

// The PC install document (docs/hosting-pc.md, #195) is checked against the
// binary three ways: every switchboard command line in it (Windows and
// Linux) uses only commands and options its --help lists; the Linux variant
// runs end to end in a temporary HOME with the real tmux backend, the fake
// agent as claude, and stubs only for what leaves the machine (GitHub
// downloads, sudo, systemd); and on Windows steps W3 to W5 run in
// PowerShell (TestE2E_HostingDocWindows).

const hostingDoc = "../../docs/hosting-pc.md"

// docBlock is one fenced code block: its language and its lines.
type docBlock struct {
	section string // "Windows", "Linux" or another ## heading
	step    string // the ### heading above it, e.g. "W3. Data directory, binary and port"
	lang    string
	lines   []string
	// prints is the text the "Check: prints `…`" line after the block
	// names ("" when there is none).
	prints string
}

// docPrints is the "Check: prints" text of the blocks of step (prefix such
// as "L4."), in order; it fails the test when there is none.
func docPrints(t *testing.T, step string) []string {
	t.Helper()
	var out []string
	for _, b := range readDocBlocks(t) {
		if strings.HasPrefix(b.step, step) && b.prints != "" {
			out = append(out, b.prints)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: no \"Check: prints\" line", step)
	}
	return out
}

// hasLine reports whether out has a line equal to want.
func hasLine(out, want string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimRight(l, "\r ") == want {
			return true
		}
	}
	return false
}

func readDocBlocks(t *testing.T) []docBlock {
	t.Helper()
	b, err := os.ReadFile(hostingDoc)
	if err != nil {
		t.Fatal(err)
	}
	var out []docBlock
	var cur *docBlock
	section, step := "", ""
	const prints = "Check: prints `"
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		switch {
		case cur == nil && strings.HasPrefix(line, "## "):
			section, step = strings.TrimPrefix(line, "## "), ""
		case cur == nil && strings.HasPrefix(line, "### "):
			step = strings.TrimPrefix(line, "### ")
		case cur == nil && strings.HasPrefix(line, prints) && len(out) > 0 && out[len(out)-1].step == step && out[len(out)-1].prints == "":
			text, _, _ := strings.Cut(strings.TrimPrefix(line, prints), "`")
			out[len(out)-1].prints = text
		case cur == nil && strings.HasPrefix(line, "```"):
			cur = &docBlock{section: section, step: step, lang: strings.TrimPrefix(line, "```")}
		case cur != nil && line == "```":
			out = append(out, *cur)
			cur = nil
		case cur != nil:
			cur.lines = append(cur.lines, line)
		}
	}
	if cur != nil {
		t.Fatal("unclosed code block in " + hostingDoc)
	}
	return out
}

// docInvocation finds switchboard command lines: sbpc …, & $sb …, "$SB" …
// and the binary named in up.cmd or the systemd unit.
var docInvocation = regexp.MustCompile(`(?:^|[\s(|;{=])(?:sbpc|& \$sb|"\$SB"|\$SB|bin\\switchboard\.exe"|bin/switchboard)\s+`)

// docArgs splits the rest of a command line into words, stopping at a
// pipe, redirection or the end of a subexpression.
func docArgs(s string) []string {
	var words []string
	var w strings.Builder
	var quote rune
	inWord := false
	flush := func() {
		if inWord {
			words = append(words, w.String())
		}
		w.Reset()
		inWord = false
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				w.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			flush()
		case r == '|' || r == '<' || r == '>' || r == ')' || r == ';' || r == '}':
			flush()
			return words
		default:
			w.WriteRune(r)
			inWord = true
		}
	}
	flush()
	return words
}

// helpOptions runs `bin CMD --help` and returns its usage line and options.
func helpOptions(t *testing.T, bin, cmd string) (string, map[string]bool) {
	t.Helper()
	out, err := exec.Command(bin, cmd, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("%s --help: %v\n%s", cmd, err, out)
	}
	lines := strings.Split(string(out), "\n")
	if !strings.HasPrefix(lines[0], "Usage: ") || !strings.Contains(lines[0], " "+cmd) {
		t.Fatalf("%s --help is not the command's help:\n%s", cmd, out)
	}
	opts := map[string]bool{}
	for _, l := range lines {
		if f := strings.Fields(l); len(f) > 0 && strings.HasPrefix(l, "  --") {
			opts[f[0]] = true
		}
	}
	return lines[0], opts
}

func TestHostingDocCommandsMatchHelp(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	dir := t.TempDir()
	bin := e2eBinary(t, dir)
	type helpOf struct {
		usage string
		opts  map[string]bool
	}
	helps := map[string]helpOf{}
	seen := map[string]map[string]bool{"Windows": {}, "Linux": {}}
	for _, b := range readDocBlocks(t) {
		for _, line := range b.lines {
			for _, m := range docInvocation.FindAllStringIndex(line, -1) {
				words := docArgs(line[m[1]:])
				// The global --profile NAME may come first.
				for len(words) >= 2 && words[0] == "--profile" {
					words = words[2:]
				}
				if len(words) == 0 {
					t.Errorf("%s: no command in %q", b.section, line)
					continue
				}
				cmd := words[0]
				if strings.ContainsAny(cmd[:1], "$@{") {
					continue // the helper's own definition: sbpc passes its arguments on
				}
				h, ok := helps[cmd]
				if !ok {
					h.usage, h.opts = helpOptions(t, bin, cmd)
					helps[cmd] = h
				}
				for i, w := range words[1:] {
					if w == "--" {
						break
					}
					if strings.HasPrefix(w, "--") && w != "--help" && w != "--profile" && !h.opts[w] {
						t.Errorf("%s: %q: %s is not an option of %s (%s)", b.section, line, w, cmd, h.usage)
					}
					// The action word of host/admin/config must be in the usage line.
					if i == 0 && (cmd == "host" || cmd == "admin" || cmd == "config") && !strings.Contains(h.usage, w) {
						t.Errorf("%s: %q: %s is not an action of %s (%s)", b.section, line, w, cmd, h.usage)
					}
				}
				if s := seen[b.section]; s != nil {
					key := cmd
					if len(words) > 1 && (cmd == "host" || cmd == "admin" || cmd == "config") {
						key += " " + words[1]
					}
					s[key] = true
				}
			}
		}
	}
	want := []string{"version", "up", "admin create", "register", "config set", "inbox", "host add", "host list", "host remove"}
	for section, s := range seen {
		for _, w := range want {
			if !s[w] {
				t.Errorf("%s: no %q command line found (found %v)", section, w, keys(s))
			}
		}
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestE2E_HostingDocLinux runs every sh block of the Linux variant, in
// order, as one bash script.
func TestE2E_HostingDocLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the Linux variant runs on Linux")
	}
	if testing.Short() {
		t.Skip("end to end: not in -short")
	}
	for _, tool := range []string{"bash", "python3", "curl", "sha256sum", "tar", "sed", "sort"} {
		if _, err := exec.LookPath(tool); err != nil {
			e2eSkip(t, "%s unavailable: %v", tool, err)
		}
	}
	if _, err := tmux.Check(context.Background(), ""); err != nil {
		e2eSkip(t, "tmux unavailable: %v", err)
	}
	var script strings.Builder
	script.WriteString("set -euo pipefail\n")
	inherited := false
	l4 := 0
	for _, b := range readDocBlocks(t) {
		if b.section == "Linux" {
			if b.lang != "sh" {
				t.Fatalf("Linux block in %q, want sh", b.lang)
			}
			if strings.HasPrefix(b.step, "L4.") {
				if l4++; l4 == 2 {
					script.WriteString(personAtSetupPageSh)
				}
			}
			if strings.HasPrefix(b.step, "L5.") && !inherited {
				// What an agent session's shell carries; L5 clears it.
				script.WriteString("export SWITCHBOARD_PROFILE=elsewhere SWITCHBOARD_ROLE=team/other AGENT_MAIL_PROFILE=elsewhere\n")
				inherited = true
			}
			fmt.Fprintf(&script, "echo '>>> %s'\n%s\n", strings.ReplaceAll(b.lines[0], "'", ""), strings.Join(b.lines, "\n"))
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	text := script.String()
	if !strings.Contains(text, "SBPORT=8090\n") {
		t.Fatal("the Linux variant does not set SBPORT=8090")
	}
	text = strings.ReplaceAll(text, "SBPORT=8090\n", "SBPORT="+port+"\n")

	root, err := os.MkdirTemp("", "sbdoc")
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	stubs := filepath.Join(root, "stubs")
	release := filepath.Join(root, "release")
	tmuxDir := filepath.Join(root, "tmux")
	for _, d := range []string{home, stubs, release, tmuxDir, filepath.Join(root, "systemd")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	data := filepath.Join(home, ".local", "share", "switchboard-pc")
	t.Cleanup(func() {
		// What the script would have ended: the unit's up, the tmux server.
		if b, err := os.ReadFile(filepath.Join(root, "systemd", "switchboard-pc.service.pid")); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				if p, err := os.FindProcess(pid); err == nil {
					_ = p.Kill()
				}
			}
		}
		kill := exec.Command("tmux", "-L", "switchboard-"+instanceFor(filepath.Join(data, upNodeToken)), "kill-server")
		kill.Env = append(os.Environ(), "TMUX_TMPDIR="+tmuxDir)
		_ = kill.Run()
		_ = os.RemoveAll(root)
	})

	// The release: this tree's binary as the archive the doc downloads.
	bin := e2eBinary(t, filepath.Join(root, "build"))
	tag := "v0.0.0-doc"
	asset := "switchboard_" + tag + "_linux_" + runtime.GOARCH + ".tar.gz"
	archive := hostingDocArchive(t, bin)
	if err := os.WriteFile(filepath.Join(release, asset), archive, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive)
	checksums := hex.EncodeToString(sum[:]) + "  " + asset + "\n" +
		strings.Repeat("0", 64) + "  switchboard_" + tag + "_windows_amd64.zip\n"
	for name, body := range map[string]string{
		"checksums.txt": checksums,
		"latest.json":   `{"tag_name":"` + tag + `"}`,
	} {
		if err := os.WriteFile(filepath.Join(release, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	realCurl, err := exec.LookPath("curl")
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"claude":    "#!/bin/sh\nexec '" + exe + "' \"$@\"\n",
		"sudo":      "#!/bin/sh\necho \"sudo $*\" >> '" + filepath.Join(root, "sudo.log") + "'\n",
		"curl":      strings.NewReplacer("__REAL__", realCurl, "__REL__", release, "__TAG__", tag).Replace(stubCurl),
		"systemctl": strings.ReplaceAll(stubSystemctl, "__STATE__", filepath.Join(root, "systemd")),
	} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	scriptFile := filepath.Join(root, "doc.sh")
	if err := os.WriteFile(scriptFile, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}

	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "SWITCHBOARD_") || strings.HasPrefix(name, "AGENT_MAIL_") || name == "DB_PATH" || name == "LISTEN_ADDR" ||
			name == "HOME" || name == "PATH" || name == "TMUX" || strings.HasPrefix(name, "XDG_") || name == "TMUX_TMPDIR" {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+home, "PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"), "TMUX_TMPDIR="+tmuxDir,
		"SWITCHBOARD_FAKE_AGENT_HEARTBEAT=200ms")
	env = append(env, fakeagent.Env(filepath.Join(root, "transcripts"))...)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", scriptFile)
	cmd.Dir = home
	cmd.Env = env
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	upLog, _ := os.ReadFile(filepath.Join(data, "up.log"))
	t.Logf("Linux variant output:\n%s", out)
	if err != nil {
		t.Fatalf("the Linux variant failed: %v\n%s\nup.log:\n%s", err, out, upLog)
	}
	for _, want := range append(docPrints(t, "L4."), docPrints(t, "L5.")[0], "PERSON person", "404", "no setup-code") {
		if !hasLine(string(out), want) {
			t.Errorf("output has no line %q (the doc's check):\n%s", want, out)
		}
	}
	for _, want := range []string{
		asset + ": OK",
		`{"role":"team/worker","status":"registered"}`,
		"pc-worker.json",
		"terminal hosting is on",
		"tmux backend",
		"switchboard listening on 127.0.0.1:" + port,
		"team/worker-1 online",
		"same process",
		"resumed",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Errorf("uninstall left the data directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "systemd", "user", "switchboard-pc.service")); !os.IsNotExist(err) {
		t.Errorf("uninstall left the unit: %v", err)
	}
	has := exec.Command("tmux", "-L", "switchboard-"+instanceFor(filepath.Join(data, upNodeToken)), "has-session")
	has.Env = append(os.Environ(), "TMUX_TMPDIR="+tmuxDir)
	if has.Run() == nil {
		t.Error("uninstall left the tmux server running")
	}
}

// hostingDocArchive is a release tar.gz holding bin as "switchboard".
func hostingDocArchive(t *testing.T, bin string) []byte {
	t.Helper()
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "switchboard", Mode: 0o755, Size: int64(len(b)), ModTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// stubCurl serves the GitHub release URLs from the local release directory
// and passes every other URL to the real curl.
const stubCurl = `#!/bin/bash
url= out=
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  case "${args[i]}" in
    -o) out="${args[i+1]}"; i=$((i + 1)) ;;
    https://*) url="${args[i]}" ;;
  esac
done
case "$url" in
  https://api.github.com/repos/sandcorporation/switchboard/releases/latest) file='__REL__/latest.json' ;;
  https://github.com/sandcorporation/switchboard/releases/download/__TAG__/*) file="__REL__/${url##*/}" ;;
  https://*) echo "curl stub: no network in this test: $url" >&2; exit 6 ;;
  *) exec '__REAL__' "$@" ;;
esac
[ -f "$file" ] || { echo "curl: (22) The requested URL returned error: 404" >&2; exit 22; }
if [ -n "$out" ]; then cp "$file" "$out"; else cat "$file"; fi
`

// stubSystemctl plays systemd for one user unit: it starts ExecStart with
// the unit's Environment lines and appends to its StandardOutput file, and
// stops only that process (KillMode=process).
const stubSystemctl = `#!/bin/bash
state='__STATE__'
[ "${1-}" = --user ] && shift
cmd="${1-}"; shift || true
now=0; unit=
for a in "$@"; do case "$a" in --now) now=1 ;; *) unit="$a" ;; esac; done
file="$HOME/.config/systemd/user/$unit"
pidf="$state/$unit.pid"
running() { [ -f "$pidf" ] && kill -0 "$(cat "$pidf")" 2>/dev/null; }
start() {
  running && return 0
  [ -f "$file" ] || { echo "Failed to start $unit: Unit $unit not found." >&2; return 5; }
  local envs=() exe= out=/dev/null errf=/dev/null line
  while IFS= read -r line; do
    case "$line" in
      Environment=*) envs+=("${line#Environment=}") ;;
      ExecStart=*) exe="${line#ExecStart=}" ;;
      StandardOutput=append:*) out="${line#StandardOutput=append:}" ;;
      StandardError=append:*) errf="${line#StandardError=append:}" ;;
    esac
  done < "$file"
  grep -qx 'KillMode=process' "$file" || { echo "systemctl stub: $unit lacks KillMode=process" >&2; return 1; }
  env "${envs[@]}" $exe </dev/null >>"$out" 2>>"$errf" &
  echo $! > "$pidf"
}
stop() {
  running || { rm -f "$pidf"; return 0; }
  local p; p="$(cat "$pidf")"
  kill "$p"
  for _ in $(seq 150); do kill -0 "$p" 2>/dev/null || break; sleep 0.1; done
  rm -f "$pidf"
}
case "$cmd" in
  daemon-reload) ;;
  enable) if [ "$now" = 1 ]; then start; fi ;;
  disable) if [ "$now" = 1 ]; then stop; fi ;;
  start) start ;;
  stop) stop ;;
  restart) stop; start ;;
  is-active|status) running ;;
  *) echo "systemctl stub: unsupported $cmd" >&2; exit 1 ;;
esac
`

// TestE2E_HostingDocWindows runs steps W3 to W6 of the Windows variant in
// Windows PowerShell: W3 with a release zip of this tree's binary and the
// fake agent as claude.exe on PATH, W4 and W5 as written (admin, the setup
// run of up, register, profiles), W6 (up.cmd, the scheduled task, builtin).
// Then, with up running from up.cmd, the OIDC sign-in block against a fake
// provider and W6 again (the way back): both rewrite up.cmd, and up.log
// must show exactly one start per block, in the new mode (#240: a batch
// file rewritten under a running cmd.exe restarted the old up). Then the
// doc's Get-SbTotp is compared with the server's TOTP. W1, W2 (network)
// and W7 to W9 (a real agent) are not run.
func TestE2E_HostingDocWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the Windows variant runs on Windows")
	}
	if testing.Short() {
		t.Skip("end to end: not in -short")
	}
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		e2eSkip(t, "powershell.exe unavailable: %v", err)
	}
	b := e2eBackendFor(t)
	var blocks []string
	var w6, oidcBlock string
	for _, d := range readDocBlocks(t) {
		if d.section == "Windows" && strings.HasPrefix(d.step, "W6.") && w6 == "" {
			w6 = "Write-Output '>>> W6.'\n" + strings.Join(d.lines, "\n")
		}
		if d.section == "OIDC sign-in" && d.lang == "powershell" {
			oidcBlock = "Write-Output '>>> OIDC sign-in'\n" + strings.Join(d.lines, "\n")
		}
		if d.section == "Windows" && (strings.HasPrefix(d.step, "W3.") || strings.HasPrefix(d.step, "W4.") || strings.HasPrefix(d.step, "W5.")) {
			pre := ""
			if strings.HasPrefix(d.step, "W5.") && !strings.Contains(strings.Join(blocks, "\n"), ">>> W5.") {
				// What an agent session's window carries; W5 clears it.
				pre = "$env:SWITCHBOARD_PROFILE = 'elsewhere'; $env:SWITCHBOARD_ROLE = 'team/other'; $env:AGENT_MAIL_PROFILE = 'elsewhere'\n"
			}
			if strings.HasPrefix(d.step, "W4.") && strings.Contains(strings.Join(blocks, "\n"), ">>> W4.") {
				pre = personAtSetupPagePS
			}
			blocks = append(blocks, pre+"Write-Output '>>> "+d.step+"'\n"+strings.Join(d.lines, "\n"))
		}
	}
	if len(blocks) != 6 {
		t.Fatalf("W3 to W5 have %d blocks, want 6", len(blocks))
	}
	if w6 == "" || oidcBlock == "" {
		t.Fatal("no W6 or Windows OIDC sign-in block")
	}
	idp := newFakeIdP(t)
	for _, r := range [][2]string{{"$issuer = 'https://id.example/realms/home'\n", "$issuer = '" + idp.srv.URL + "'\n"}, {"$clientId = '<client-id>'\n", "$clientId = '" + idp.clientID + "'\n"}} {
		if !strings.Contains(oidcBlock, r[0]) {
			t.Fatalf("the OIDC sign-in block has no line %q", r[0])
		}
		oidcBlock = strings.Replace(oidcBlock, r[0], r[1], 1)
	}
	blocks = append(blocks, w6, oidcBlock, strings.Replace(w6, ">>> W6.", ">>> W6. again", 1))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	root, err := os.MkdirTemp("", "sbdocw")
	if err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(root, "local", "switchboard-pc")
	t.Cleanup(func() {
		// What the script leaves running: the task's up and the task.
		stop := "Get-NetTCPConnection -LocalPort " + port + " -State Listen -ErrorAction SilentlyContinue | ForEach-Object { Stop-Process -Id $_.OwningProcess -Force }\n" +
			"Unregister-ScheduledTask -TaskName 'switchboard-pc' -Confirm:$false -ErrorAction SilentlyContinue\n"
		if out, err := exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command", stop).CombinedOutput(); err != nil {
			t.Logf("cleanup: %v\n%s", err, out)
		}
		b.end(instanceFor(filepath.Join(data, upNodeToken)))
		deadline := time.Now().Add(15 * time.Second)
		for os.RemoveAll(root) != nil && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
		}
	})
	agentDir, dl := filepath.Join(root, "agent"), filepath.Join(root, "dl")
	for _, d := range []string{agentDir, dl, filepath.Join(root, "local")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "claude.exe"), agent, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := e2eBinary(t, filepath.Join(root, "build"))
	asset := "switchboard_v0.0.0-doc_windows_" + runtime.GOARCH + ".zip"
	if err := os.WriteFile(filepath.Join(dl, asset), hostingDocZip(t, bin), 0o600); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(blocks, "\n")
	if !strings.Contains(body, "$port = 8090\n") {
		t.Fatal("W3 does not set $port = 8090")
	}
	body = strings.ReplaceAll(body, "$port = 8090\n", "$port = "+port+"\n")
	secret := totpEncoding.EncodeToString([]byte("0123456789abcdefghij"))
	script := "$ErrorActionPreference = 'Stop'\n" +
		"$env:LOCALAPPDATA = '" + filepath.Join(root, "local") + "'\n" +
		"$env:PATH = '" + agentDir + ";' + $env:PATH\n" +
		"$dl = '" + dl + "'\n$asset = '" + asset + "'\n" +
		body + "\n'TOTP ' + (Get-SbTotp '" + secret + "')\n" +
		"'LEFT[' + ((Get-ChildItem Env: | Where-Object Name -match '^(SWITCHBOARD|AGENT_MAIL)_(PROFILE|ROLE)$').Name -join ',') + ']'\n"
	file := filepath.Join(root, "doc.ps1")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file)
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if n := strings.ToUpper(name); strings.HasPrefix(n, "SWITCHBOARD_") || strings.HasPrefix(n, "AGENT_MAIL_") || n == "DB_PATH" || n == "LISTEN_ADDR" {
			continue
		}
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Env = append(cmd.Env, fakeagent.Env(filepath.Join(root, "transcripts"))...)
	before := time.Now().Unix() / adminauth.TOTPStep
	out, err := cmd.CombinedOutput()
	after := time.Now().Unix() / adminauth.TOTPStep
	setupLog, _ := os.ReadFile(filepath.Join(data, "setup.log"))
	upLog, _ := os.ReadFile(filepath.Join(data, "up.log"))
	t.Logf("W3 to W6 output:\n%s", out)
	if err != nil {
		t.Fatalf("W3 to W6: %v\n%s\nsetup.log:\n%s\nup.log:\n%s", err, out, setupLog, upLog)
	}
	// One start per block that wrote up.cmd, each in that block's mode: no
	// second start of the old up from the rewritten file.
	var modes []string
	for _, l := range strings.Split(string(upLog), "\n") {
		if _, m, ok := strings.Cut(strings.TrimRight(l, "\r"), "admin auth: "); ok {
			modes = append(modes, m)
		}
	}
	if want := []string{"builtin", "oidc (public client, PKCE)", "builtin"}; !slices.Equal(modes, want) {
		t.Errorf("up.log admin auth lines %q, want %q\nup.log:\n%s", modes, want, upLog)
	}
	// sbstarted: the mode line and /health's version (e2eBinary's "e2e").
	for _, want := range []string{"admin auth: builtin", "admin auth: oidc (public client, PKCE)", "version e2e"} {
		if !hasLine(string(out), want) {
			t.Errorf("output has no line %q (sbstarted)", want)
		}
	}
	if want := "redirect_uri=http%3A%2F%2F127.0.0.1%3A" + port + "%2Fauth%2Fcallback"; !strings.Contains(string(out), idp.srv.URL+"/authorize?") || !strings.Contains(string(out), want) {
		t.Errorf("/auth/login did not redirect to the provider with %s", want)
	}
	for _, want := range append(docPrints(t, "W4."), "LEFT[]", "PERSON person", "404", "False") {
		if !hasLine(string(out), want) {
			t.Errorf("output has no line %q", want)
		}
	}
	for _, want := range []string{
		" e2e",
		`{"role":"team/boss","status":"registered"}`,
		`{"role":"team/worker","status":"registered"}`,
		"pc-boss.json",
		"pc-worker.json",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q", want)
		}
	}
	for _, f := range []string{"enroll.token", "boss.token", "worker.token"} {
		if _, err := os.Stat(filepath.Join(data, f)); !os.IsNotExist(err) {
			t.Errorf("W5 left %s: %v", f, err)
		}
	}
	var ok []string
	for step := before; step <= after; step++ {
		code, err := adminauth.TOTPCode(secret, step)
		if err != nil {
			t.Fatal(err)
		}
		ok = append(ok, "TOTP "+code)
	}
	if !slices.ContainsFunc(ok, func(s string) bool { return strings.Contains(string(out), s) }) {
		t.Errorf("Get-SbTotp printed none of %v", ok)
	}
}

// hostingDocZip is a release zip holding bin as "switchboard.exe".
func hostingDocZip(t *testing.T, bin string) []byte {
	t.Helper()
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("switchboard.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// personAtSetupPageSh and personAtSetupPagePS stand in for the person at the
// browser between the two L4/W4 blocks: the setup page's own requests with
// the code from the file, then the TOTP code of the secret it got back.
const personAtSetupPageSh = `echo '>>> person at the setup page'
SBCODE="$(cat "$SBDATA/setup-code")"
SBBEGUN="$(curl -fsS -H 'Content-Type: application/json' -H 'X-Switchboard-Dashboard: 1' -d "{\"code\":\"$SBCODE\",\"name\":\"person\",\"password\":\"person password 12\"}" "http://127.0.0.1:$SBPORT/setup/begin")"
SBPTOTP="$(printf '%s' "$SBBEGUN" | python3 -c 'import base64,hashlib,hmac,json,struct,sys,time
s=json.load(sys.stdin)["secret"]; k=base64.b32decode(s+"="*(-len(s)%8))
h=hmac.new(k,struct.pack(">Q",int(time.time())//30),hashlib.sha1).digest(); o=h[19]&15
print("%06d"%((struct.unpack(">I",h[o:o+4])[0]&0x7fffffff)%1000000))')"
curl -fsS -H 'Content-Type: application/json' -H 'X-Switchboard-Dashboard: 1' -d "{\"code\":\"$SBCODE\",\"totp\":\"$SBPTOTP\"}" "http://127.0.0.1:$SBPORT/setup/confirm" |
  python3 -c 'import json,sys; print("PERSON " + json.load(sys.stdin)["user"])'
`

const personAtSetupPagePS = `Write-Output '>>> person at the setup page'
$sbcode = (Get-Content "$data\setup-code").Trim()
$sbh = @{ 'X-Switchboard-Dashboard' = '1' }
$sbbegun = Invoke-RestMethod -Method Post "http://127.0.0.1:$port/setup/begin" -ContentType 'application/json' -Headers $sbh -Body (@{ code = $sbcode; name = 'person'; password = 'person password 12' } | ConvertTo-Json)
$sbdone = Invoke-RestMethod -Method Post "http://127.0.0.1:$port/setup/confirm" -ContentType 'application/json' -Headers $sbh -Body (@{ code = $sbcode; totp = (Get-SbTotp $sbbegun.secret) } | ConvertTo-Json)
'PERSON ' + $sbdone.user
`
