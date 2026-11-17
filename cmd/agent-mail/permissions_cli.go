package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// cliPermission serves agent-mail permission request|show|answer|list.
func cliPermission(c Config, p parsedArgs) (int, error) {
	if len(p.pos) == 0 {
		return 0, usagef("permission: missing action (request, show, answer, list)")
	}
	action := p.pos[0]
	needID := action == "show" || action == "answer"
	switch {
	case action != "request" && action != "list" && !needID:
		return 0, usagef("permission: unknown action %q (request, show, answer, list)", action)
	case needID && len(p.pos) != 2:
		return 0, usagef("permission %s: expected a request ID", action)
	case !needID && len(p.pos) != 1:
		return 0, usagef("permission %s: unexpected argument %q", action, p.pos[1])
	}
	var b []byte
	var err error
	switch action {
	case "request":
		if p.values["--tool"] == "" || p.values["--summary"] == "" {
			return 0, usagef("permission request: --tool and --summary are required")
		}
		b, err = slotCall(c, "POST", "/api/permissions", map[string]string{"tool": p.values["--tool"], "summary": p.values["--summary"]})
	case "show":
		b, err = slotCall(c, "GET", "/api/permissions/"+url.PathEscape(p.pos[1]), nil)
	case "answer":
		d := p.values["--decision"]
		if d != "allow" && d != "deny" {
			return 0, usagef("permission answer: --decision must be allow or deny")
		}
		b, err = slotCall(c, "POST", "/api/permissions/"+url.PathEscape(p.pos[1])+"/answer", map[string]string{"decision": d, "note": p.values["--note"]})
	case "list":
		path := "/api/permissions"
		if p.bool("--pending") {
			path += "?pending=1"
		}
		b, err = slotCall(c, "GET", path, nil)
	}
	if err != nil {
		return 0, err
	}
	if p.bool("--json") {
		fmt.Println(strings.TrimSpace(string(b)))
		return exitOK, nil
	}
	if action == "list" {
		var out permissionList
		if err = json.Unmarshal(b, &out); err != nil {
			return 0, fmt.Errorf("unexpected response from server: %v", err)
		}
		if len(out.Permissions) == 0 {
			fmt.Println("no permission requests")
		}
		for _, v := range out.Permissions {
			fmt.Println(permissionLine(v))
		}
		return exitOK, nil
	}
	var v permissionView
	if err = json.Unmarshal(b, &v); err != nil {
		return 0, fmt.Errorf("unexpected response from server: %v", err)
	}
	fmt.Println(permissionLine(v))
	return exitOK, nil
}

// permissionLine: "<id> <status> <requester> <tool> | <summary>[ | by X][ | note]".
func permissionLine(v permissionView) string {
	line := fmt.Sprintf("%s  %s  %s  %s | %s", v.ID, v.Status, v.Requester, v.Tool, v.Summary)
	if v.AnsweredBy != "" {
		line += " | by " + v.AnsweredBy
	}
	if v.Note != "" {
		line += " | note: " + v.Note
	}
	return line
}
