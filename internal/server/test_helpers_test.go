package server

import (
	"os"
	"strconv"

	"github.com/sandcorporation/switchboard/internal/adminauth"
	"github.com/sandcorporation/switchboard/internal/cli"
)

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(i int64) any {
	if i == 0 {
		return nil
	}
	return i
}

// Limits and names the tests check, as a client sees them.
const (
	maxPermissionTool = 128 // bytes
	maxPermissionText = 500 // bytes, for summary and note
	ndjsonMimeType    = "application/x-ndjson"
)

// discordPayload is the webhook body a Discord notice posts.
type discordPayload struct {
	Content         string `json:"content"`
	Username        string `json:"username,omitempty"`
	AvatarURL       string `json:"avatar_url,omitempty"`
	AllowedMentions struct {
		Parse []string `json:"parse"`
	} `json:"allowed_mentions"`
}

// runMain is cmd/switchboard's main: the server without arguments, else the
// client.
func runMain() {
	args := append([]string(nil), os.Args[1:]...)
	if len(args) == 0 {
		Run(version)
		return
	}
	os.Exit(cli.Run(args, version, adminauth.CLI))
}
