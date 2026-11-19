//go:build windows

package conpty

import (
	"context"
	"net"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// PipeName is the keeper's named pipe for an instance.
func PipeName(instance string) string { return `\\.\pipe\switchboard-keeper-` + instance }

// UserOnlySDDL is the pipe's security descriptor: a protected DACL with one
// entry granting the current user's SID full access, so no other account
// (administrators included) can open the pipe.
func UserOnlySDDL() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return "D:P(A;;GA;;;" + u.User.Sid.String() + ")", nil
}

// Listen creates the instance's pipe. It fails while another keeper holds
// the name.
func Listen(instance string) (net.Listener, error) {
	sd, err := UserOnlySDDL()
	if err != nil {
		return nil, err
	}
	return winio.ListenPipe(PipeName(instance), &winio.PipeConfig{SecurityDescriptor: sd})
}

// DialFor reaches the instance's keeper.
func DialFor(instance string) Dialer {
	return func(ctx context.Context) (net.Conn, error) {
		return winio.DialPipeContext(ctx, PipeName(instance))
	}
}
