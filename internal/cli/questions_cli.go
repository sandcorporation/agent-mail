package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/sandcorporation/switchboard/internal/wire"
)

// cliQuestion serves switchboard question ask|show|list|answer|withdraw.
func cliQuestion(c Config, p parsedArgs) (int, error) {
	const actions = "ask, show, list, answer, withdraw"
	if len(p.pos) == 0 {
		return 0, usagef("question: missing action (%s)", actions)
	}
	action := p.pos[0]
	needID := action == "show" || action == "answer" || action == "withdraw"
	switch {
	case action != "ask" && action != "list" && !needID:
		return 0, usagef("question: unknown action %q (%s)", action, actions)
	case needID && len(p.pos) != 2:
		return 0, usagef("question %s: expected a question ID", action)
	case !needID && len(p.pos) != 1:
		return 0, usagef("question %s: unexpected argument %q", action, p.pos[1])
	}
	var b []byte
	var err error
	switch action {
	case "ask":
		body, e := questionFile(p, "ask", "questions")
		if e != nil {
			return 0, e
		}
		b, err = slotCall(c, "POST", "/api/questions", body)
	case "show":
		b, err = slotCall(c, "GET", "/api/questions/"+url.PathEscape(p.pos[1]), nil)
	case "answer":
		body, e := questionFile(p, "answer", "answers")
		if e != nil {
			return 0, e
		}
		b, err = slotCall(c, "POST", "/api/questions/"+url.PathEscape(p.pos[1])+"/answer", body)
	case "withdraw":
		b, err = slotCall(c, "POST", "/api/questions/"+url.PathEscape(p.pos[1])+"/withdraw", nil)
	case "list":
		path := "/api/questions"
		if st := p.values["--status"]; st != "" {
			if st != "pending" && st != "answered" && st != "withdrawn" {
				return 0, usagef("question list: --status must be pending, answered or withdrawn")
			}
			path += "?status=" + st
		}
		b, err = slotCall(c, "GET", path, nil)
	}
	if err != nil {
		return 0, err
	}
	if p.bool("--json") {
		fmt.Println(strings.TrimSpace(string(b)))
		return ExitOK, nil
	}
	if action == "list" {
		var out wire.QuestionList
		if err = json.Unmarshal(b, &out); err != nil {
			return 0, fmt.Errorf("unexpected response from server: %v", err)
		}
		if len(out.Questions) == 0 {
			fmt.Println("no questions")
		}
		for _, v := range out.Questions {
			fmt.Println(questionLine(v))
		}
		return ExitOK, nil
	}
	var v wire.QuestionView
	if err = json.Unmarshal(b, &v); err != nil {
		return 0, fmt.Errorf("unexpected response from server: %v", err)
	}
	fmt.Println(questionLine(v))
	if action == "show" {
		fmt.Println(questionDetail(v))
	}
	return ExitOK, nil
}

// questionFile reads --file (JSON): either {"<key>":[...]} or a bare array.
func questionFile(p parsedArgs, action, key string) (json.RawMessage, error) {
	f := p.values["--file"]
	if f == "" {
		return nil, usagef("question %s: --file is required", action)
	}
	raw, err := os.ReadFile(f)
	if err != nil {
		return nil, err
	}
	raw = bytes.TrimSpace(raw)
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%s: not valid JSON", f)
	}
	if len(raw) > 0 && raw[0] == '[' {
		return json.Marshal(map[string]json.RawMessage{key: raw})
	}
	return raw, nil
}

// questionLine: "<id>  <status>  <asker>  <first header> | <first question>[ | by X]".
func questionLine(v wire.QuestionView) string {
	header, text := "", ""
	if len(v.Questions) > 0 {
		header, text = v.Questions[0].Header, v.Questions[0].Question
	}
	line := fmt.Sprintf("%s  %s  %s  %s | %s", v.ID, v.Status, v.Asker, header, text)
	if len(v.Questions) > 1 {
		line += fmt.Sprintf(" (+%d)", len(v.Questions)-1)
	}
	if v.AnsweredBy != "" {
		line += " | by " + v.AnsweredBy
	}
	return line
}

// questionDetail is the question text and options, then the answer when
// there is one.
func questionDetail(v wire.QuestionView) string {
	out := wire.QuestionText(v)
	if len(v.Answers) == len(v.Questions) && len(v.Answers) > 0 {
		out += "\n\n" + wire.AnswerText(v)
	}
	return out
}
