package service

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

var update = flag.Bool("update", false, "rewrite golden files")

func unixUnit() Unit {
	return Unit{
		Name:    "dev.example.host-agent",
		Exec:    "/opt/agent/bin/host agent",
		Args:    []string{"run", "--label", "50% & <done>", "$HOME"},
		Env:     []string{"AGENT_URL=https://agent.example.test", `NOTE=a "quoted" 100% $x`},
		WorkDir: "/var/lib/agent",
		LogPath: "/var/log/agent/host.log",
	}
}

func windowsUnit() Unit {
	return Unit{
		Name:    "dev.example.host-agent",
		Exec:    `C:\Program Files\Agent\host.exe`,
		Args:    []string{"run", "--label", "50% & <done>"},
		Env:     []string{"AGENT_URL=https://agent.example.test", "NOTE=100% & more"},
		WorkDir: `C:\Users\me\agent`,
		LogPath: `C:\Users\me\agent\host.log`,
		User:    `EXAMPLE\me`,
	}
}

// goldenText makes a rendered file readable: UTF-16 task XML is decoded.
func goldenText(b []byte) []byte {
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		units := make([]uint16, 0, len(b)/2)
		for i := 2; i+1 < len(b); i += 2 {
			units = append(units, uint16(b[i])|uint16(b[i+1])<<8)
		}
		return []byte(string(utf16.Decode(units)))
	}
	return b
}

func TestRenderGolden(t *testing.T) {
	cases := []struct {
		goos string
		unit Unit
	}{
		{"linux", unixUnit()},
		{"darwin", unixUnit()},
		{"windows", windowsUnit()},
	}
	for _, c := range cases {
		t.Run(c.goos, func(t *testing.T) {
			files, err := Render(c.goos, c.unit)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				golden := filepath.Join("testdata", c.goos, filepath.Base(f.Path)+".golden")
				got := goldenText(f.Content)
				if *update {
					if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(golden, got, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				want, err := os.ReadFile(golden)
				if err != nil {
					t.Fatalf("%v (run with -update to create)", err)
				}
				if string(got) != string(want) {
					t.Errorf("%s differs from %s:\n%s", f.Path, golden, got)
				}
			}
		})
	}
}

func TestRenderPaths(t *testing.T) {
	want := map[string][]string{
		"linux":   {".config/systemd/user/svc.service"},
		"darwin":  {"Library/LaunchAgents/svc.plist"},
		"windows": {"AppData/Local/svc/svc.cmd", "AppData/Local/svc/svc.task.xml"},
	}
	for goos, paths := range want {
		u := Unit{Name: "svc", Exec: "/bin/true"}
		if goos == "windows" {
			u.Exec = `C:\bin\true.exe`
		}
		files, err := Render(goos, u)
		if err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
		var got []string
		for _, f := range files {
			got = append(got, f.Path)
		}
		if !reflect.DeepEqual(got, paths) {
			t.Errorf("%s paths = %v, want %v", goos, got, paths)
		}
	}
}

func TestRenderTaskIsUTF16WithBOM(t *testing.T) {
	files, err := Render("windows", windowsUnit())
	if err != nil {
		t.Fatal(err)
	}
	xml := files[1].Content
	if len(xml) < 4 || xml[0] != 0xFF || xml[1] != 0xFE || xml[2] != '<' || xml[3] != 0 {
		t.Fatalf("task XML does not start with a UTF-16LE BOM: % x", xml[:4])
	}
}

func TestRenderRejects(t *testing.T) {
	ok := Unit{Name: "svc", Exec: "/bin/true"}
	bad := map[string]func(u *Unit){
		"empty name":     func(u *Unit) { u.Name = "" },
		"slash in name":  func(u *Unit) { u.Name = "a/b" },
		"relative exec":  func(u *Unit) { u.Exec = "true" },
		"relative log":   func(u *Unit) { u.LogPath = "log.txt" },
		"newline in arg": func(u *Unit) { u.Args = []string{"a\nb"} },
		"bad env":        func(u *Unit) { u.Env = []string{"NOEQUALS"} },
		"bad env key":    func(u *Unit) { u.Env = []string{"1X=y"} },
	}
	for name, mutate := range bad {
		u := ok
		mutate(&u)
		if _, err := Render("linux", u); err == nil {
			t.Errorf("%s: Render accepted %+v", name, u)
		}
	}
	if _, err := Render("windows", Unit{Name: "svc", Exec: `C:\x.exe`, Args: []string{`say "hi"`}}); err == nil {
		t.Error("windows: Render accepted a double quote")
	}
	if _, err := Render("plan9", ok); err == nil {
		t.Error("Render accepted an unsupported OS")
	}
}

// fakeRun records commands and answers from a table keyed by the joined
// command line; unknown commands succeed with no output.
type fakeRun struct {
	calls   []string
	answers map[string]fakeAnswer
}

type fakeAnswer struct {
	out string
	err error
}

func (f *fakeRun) run(name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, line)
	if a, ok := f.answers[line]; ok {
		return a.out, a.err
	}
	return "", nil
}

func testHost(t *testing.T, goos string) (host, *fakeRun) {
	f := &fakeRun{answers: map[string]fakeAnswer{}}
	return host{goos: goos, home: t.TempDir(), uid: 501, user: `EXAMPLE\me`, run: f.run}, f
}

func TestInstallLinux(t *testing.T) {
	h, f := testHost(t, "linux")
	log := filepath.Join(t.TempDir(), "logs", "svc.log")
	res, err := h.install(Unit{Name: "svc", Exec: "/bin/true", LogPath: log})
	if err != nil {
		t.Fatal(err)
	}
	unitFile := filepath.Join(h.home, ".config/systemd/user/svc.service")
	if !reflect.DeepEqual(res.Files, []string{unitFile}) {
		t.Fatalf("Files = %v", res.Files)
	}
	if _, err := os.Stat(filepath.Dir(log)); err != nil {
		t.Fatalf("log dir not created: %v", err)
	}
	want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable svc.service",
		"systemctl --user restart svc.service",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls = %q, want %q", f.calls, want)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "loginctl enable-linger") {
		t.Fatalf("Notes = %q, want a linger note", res.Notes)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "loginctl") {
			t.Fatal("Install ran loginctl")
		}
	}
}

func TestInstallDarwin(t *testing.T) {
	h, f := testHost(t, "darwin")
	f.answers["launchctl bootout gui/501/svc"] = fakeAnswer{err: errors.New("not loaded")}
	if _, err := h.install(Unit{Name: "svc", Exec: "/bin/true"}); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(h.home, "Library/LaunchAgents/svc.plist")
	want := []string{
		"launchctl bootout gui/501/svc",
		"launchctl enable gui/501/svc",
		"launchctl bootstrap gui/501 " + plist,
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls = %q, want %q", f.calls, want)
	}
}

func TestInstallWindowsFillsUser(t *testing.T) {
	h, f := testHost(t, "windows")
	if _, err := h.install(Unit{Name: "svc", Exec: `C:\bin\svc.exe`}); err != nil {
		t.Fatal(err)
	}
	xml, err := os.ReadFile(filepath.Join(h.home, "AppData/Local/svc/svc.task.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(goldenText(xml)), `<UserId>EXAMPLE\me</UserId>`) {
		t.Fatal("task XML lacks the current user")
	}
	want := []string{
		"schtasks /Create /TN svc /XML " + filepath.Join(h.home, "AppData/Local/svc/svc.task.xml") + " /F",
		"schtasks /Run /TN svc",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls = %q, want %q", f.calls, want)
	}
}

func TestUninstallIsIdempotent(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			h, f := testHost(t, goos)
			exe := "/bin/true"
			if goos == "windows" {
				exe = `C:\bin\true.exe`
			}
			if _, err := h.install(Unit{Name: "svc", Exec: exe}); err != nil {
				t.Fatal(err)
			}
			// After the first uninstall the OS no longer knows the service.
			f.answers["launchctl bootout gui/501/svc"] = fakeAnswer{err: errors.New("no such service")}
			if err := h.uninstall("svc"); err != nil {
				t.Fatalf("first uninstall: %v", err)
			}
			if h.anyFile("svc") {
				t.Fatal("definition files left behind")
			}
			f.answers["schtasks /Delete /TN svc /F"] = fakeAnswer{err: errors.New("task not found")}
			f.answers["systemctl --user disable --now svc.service"] = fakeAnswer{err: errors.New("not found")}
			if err := h.uninstall("svc"); err != nil {
				t.Fatalf("second uninstall: %v", err)
			}
		})
	}
}

func TestStatus(t *testing.T) {
	t.Run("linux", func(t *testing.T) {
		h, f := testHost(t, "linux")
		f.answers["systemctl --user is-active svc.service"] = fakeAnswer{out: "inactive", err: errors.New("exit 3")}
		st, err := h.status("svc")
		if err != nil || st != (State{Detail: "inactive"}) {
			t.Fatalf("status = %+v, %v", st, err)
		}
		f.answers["systemctl --user is-active svc.service"] = fakeAnswer{out: "active"}
		if st, _ := h.status("svc"); !st.Running {
			t.Fatalf("status = %+v, want running", st)
		}
	})
	t.Run("darwin", func(t *testing.T) {
		h, f := testHost(t, "darwin")
		f.answers["launchctl print gui/501/svc"] = fakeAnswer{err: errors.New("Could not find service")}
		if st, _ := h.status("svc"); st.Running || st.Detail != "not loaded" {
			t.Fatalf("status = %+v", st)
		}
		f.answers["launchctl print gui/501/svc"] = fakeAnswer{out: "gui/501/svc = {\n\tactive count = 1\n\tstate = running\n\tprogram = /bin/true\n}"}
		if st, _ := h.status("svc"); !st.Running || st.Detail != "running" {
			t.Fatalf("status = %+v", st)
		}
	})
	t.Run("windows", func(t *testing.T) {
		h, f := testHost(t, "windows")
		f.answers["schtasks /Query /TN svc /FO CSV /NH"] = fakeAnswer{out: `"\svc","N/A","Running"`}
		if st, _ := h.status("svc"); !st.Running {
			t.Fatalf("status = %+v", st)
		}
		f.answers["schtasks /Query /TN svc /FO CSV /NH"] = fakeAnswer{out: `"\svc","N/A","Ready"`}
		if st, _ := h.status("svc"); st.Running || st.Detail != "Ready" {
			t.Fatalf("status = %+v", st)
		}
	})
}
