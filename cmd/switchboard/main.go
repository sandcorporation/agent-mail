// Command switchboard is the mail server (no arguments) and its client (any
// subcommand). The server is assembled in internal/server and the client
// lives in internal/cli.
package main

import (
	"os"

	"github.com/sandcorporation/switchboard/internal/adminauth"
	"github.com/sandcorporation/switchboard/internal/cli"
	"github.com/sandcorporation/switchboard/internal/server"
	"github.com/sandcorporation/switchboard/internal/term/conpty"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	// Copy the arguments so parsing never rewrites os.Args.
	args := append([]string(nil), os.Args[1:]...)
	if len(args) == 0 {
		server.Run(version)
		return
	}
	if args[0] == conpty.KeeperCommand {
		// Hidden: the Windows keeper the node starts (not in help).
		os.Exit(server.Keeper(version, args[1:]))
	}
	os.Exit(cli.Run(args, version, cli.Commands{
		Admin: adminauth.CLI,
		Host:  server.HostCLI,
		Up:    func(dir string, port int, node string) error { return server.Up(version, dir, port, node) },
		Node: func(host, tokenFile, joinCode string, restartSessions bool) error {
			return server.Node(version, host, tokenFile, joinCode, restartSessions)
		},
		NodeInvite: server.NodeInviteCLI,
	}))
}
