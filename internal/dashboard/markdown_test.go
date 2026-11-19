package dashboard

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestMarkdownRenderer runs the renderer's unit tests (markdown.test.cjs)
// under node. Without node the test is skipped, unless
// SWITCHBOARD_TEST_NODE=require (CI) makes that a failure.
func TestMarkdownRenderer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("SWITCHBOARD_TEST_NODE") == "require" {
			t.Fatal("node not found and SWITCHBOARD_TEST_NODE=require")
		}
		t.Skip("node not installed")
	}
	out, err := exec.Command(node, "--test", "markdown.test.cjs").CombinedOutput()
	if err != nil {
		t.Fatalf("node --test markdown.test.cjs: %v\n%s", err, out)
	}
}

func TestMarkdownRendererIsInlined(t *testing.T) {
	if !strings.Contains(pageHTML, "<script>\n// mdRender(src, doc)") || strings.Count(pageHTML, "function mdRender(") != 1 {
		t.Fatal("pageHTML does not inline markdown.js at the top of the script")
	}
	if strings.Contains(markdownJS, "</script") {
		t.Fatal("markdown.js must not contain a closing script tag")
	}
}
