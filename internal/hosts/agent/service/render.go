package service

import (
	"bytes"
	"encoding/xml"
	"strings"
	"unicode/utf16"
)

// --- systemd (Linux) ---

func systemdPath(name string) string { return ".config/systemd/user/" + name + ".service" }

// systemdEscape escapes the % specifiers systemd expands in every value.
func systemdEscape(s string) string { return strings.ReplaceAll(s, "%", "%%") }

// systemdQuote quotes one ExecStart word (exec) or Environment= assignment.
// ExecStart also expands $VAR, so '$' is doubled there.
func systemdQuote(s string, exec bool) string {
	s = systemdEscape(s)
	if exec {
		s = strings.ReplaceAll(s, "$", "$$")
	}
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
	return `"` + s + `"`
}

func renderSystemd(u Unit) []byte {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=" + systemdEscape(u.Name) + "\n\n")
	b.WriteString("[Service]\nType=simple\n")
	words := []string{systemdQuote(u.Exec, true)}
	for _, a := range u.Args {
		words = append(words, systemdQuote(a, true))
	}
	b.WriteString("ExecStart=" + strings.Join(words, " ") + "\n")
	if u.WorkDir != "" {
		b.WriteString("WorkingDirectory=" + systemdEscape(u.WorkDir) + "\n")
	}
	for _, kv := range u.Env {
		b.WriteString("Environment=" + systemdQuote(kv, false) + "\n")
	}
	if u.LogPath != "" {
		b.WriteString("StandardOutput=append:" + systemdEscape(u.LogPath) + "\n")
		b.WriteString("StandardError=append:" + systemdEscape(u.LogPath) + "\n")
	}
	b.WriteString("Restart=always\nRestartSec=5\n\n")
	b.WriteString("[Install]\nWantedBy=default.target\n")
	return []byte(b.String())
}

// --- launchd (macOS) ---

func launchdPath(name string) string { return "Library/LaunchAgents/" + name + ".plist" }

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s)) // writes to a bytes.Buffer never fail
	return b.String()
}

func renderLaunchd(u Unit) []byte {
	var b strings.Builder
	str := func(indent, s string) { b.WriteString(indent + "<string>" + xmlText(s) + "</string>\n") }
	key := func(k string) { b.WriteString("\t<key>" + k + "</key>\n") }

	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	key("Label")
	str("\t", u.Name)
	key("ProgramArguments")
	b.WriteString("\t<array>\n")
	str("\t\t", u.Exec)
	for _, a := range u.Args {
		str("\t\t", a)
	}
	b.WriteString("\t</array>\n")
	if len(u.Env) > 0 {
		key("EnvironmentVariables")
		b.WriteString("\t<dict>\n")
		for _, kv := range u.Env {
			k, v, _ := strings.Cut(kv, "=")
			b.WriteString("\t\t<key>" + xmlText(k) + "</key>\n")
			str("\t\t", v)
		}
		b.WriteString("\t</dict>\n")
	}
	if u.WorkDir != "" {
		key("WorkingDirectory")
		str("\t", u.WorkDir)
	}
	if u.LogPath != "" {
		key("StandardOutPath")
		str("\t", u.LogPath)
		key("StandardErrorPath")
		str("\t", u.LogPath)
	}
	key("RunAtLoad")
	b.WriteString("\t<true/>\n")
	key("KeepAlive")
	b.WriteString("\t<true/>\n")
	key("ProcessType")
	str("\t", "Background")
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String())
}

// --- Task Scheduler (Windows) ---

// The task runs a small .cmd wrapper because a scheduled task has no
// environment or log redirection of its own. Both files live in
// %LOCALAPPDATA%\<name>; the task refers to the wrapper through that
// variable, which Task Scheduler expands when it starts the action.
func windowsDir(name string) string        { return "AppData/Local/" + name }
func windowsScriptPath(name string) string { return windowsDir(name) + "/" + name + ".cmd" }
func windowsTaskPath(name string) string   { return windowsDir(name) + "/" + name + ".task.xml" }

// cmdText escapes % for a batch file. Values never contain '"' (validate
// rejects them), so quoting each value is enough for the other specials.
func cmdText(s string) string { return strings.ReplaceAll(s, "%", "%%") }

func renderWindowsScript(u Unit) []byte {
	var b strings.Builder
	b.WriteString("@echo off\r\nsetlocal\r\n")
	for _, kv := range u.Env {
		b.WriteString(`set "` + cmdText(kv) + "\"\r\n")
	}
	if u.WorkDir != "" {
		b.WriteString(`cd /d "` + cmdText(u.WorkDir) + "\"\r\n")
	}
	line := `"` + cmdText(u.Exec) + `"`
	for _, a := range u.Args {
		line += ` "` + cmdText(a) + `"`
	}
	if u.LogPath != "" {
		line += ` >> "` + cmdText(u.LogPath) + `" 2>&1`
	}
	b.WriteString(line + "\r\n")
	return []byte(b.String())
}

// renderWindowsTask returns the task definition as UTF-16LE with a BOM, the
// encoding schtasks /XML expects.
func renderWindowsTask(u Unit) []byte {
	return utf16LE(windowsTaskXML(u))
}

func windowsTaskXML(u Unit) string {
	userID := ""
	if u.User != "" {
		userID = "<UserId>" + xmlText(u.User) + "</UserId>"
	}
	lines := []string{
		`<?xml version="1.0" encoding="UTF-16"?>`,
		`<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">`,
		`  <RegistrationInfo>`,
		`    <Description>` + xmlText(u.Name) + `</Description>`,
		`  </RegistrationInfo>`,
		`  <Triggers>`,
		`    <LogonTrigger>`,
		`      <Enabled>true</Enabled>`,
	}
	if userID != "" {
		lines = append(lines, `      `+userID)
	}
	lines = append(lines,
		`    </LogonTrigger>`,
		`  </Triggers>`,
		`  <Principals>`,
		`    <Principal id="Author">`,
	)
	if userID != "" {
		lines = append(lines, `      `+userID)
	}
	lines = append(lines,
		`      <LogonType>InteractiveToken</LogonType>`,
		`      <RunLevel>LeastPrivilege</RunLevel>`,
		`    </Principal>`,
		`  </Principals>`,
		`  <Settings>`,
		`    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>`,
		`    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>`,
		`    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>`,
		`    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>`,
		`    <RestartOnFailure>`,
		`      <Interval>PT1M</Interval>`,
		`      <Count>999</Count>`,
		`    </RestartOnFailure>`,
		`  </Settings>`,
		`  <Actions Context="Author">`,
		`    <Exec>`,
		`      <Command>%LOCALAPPDATA%\`+xmlText(u.Name)+`\`+xmlText(u.Name)+`.cmd</Command>`,
		`    </Exec>`,
		`  </Actions>`,
		`</Task>`,
	)
	return strings.Join(lines, "\r\n") + "\r\n"
}

func utf16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, 2+2*len(units))
	out = append(out, 0xFF, 0xFE)
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}
