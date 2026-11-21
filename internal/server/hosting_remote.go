package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/sandcorporation/switchboard/internal/config"
)

// Remote nodes (#329): with SWITCHBOARD_HOSTING_REMOTE_LISTEN set, a hosting
// host opens a second listener on a private-network address for nodes on
// other machines. It serves the node channel and the role API to peers inside
// SWITCHBOARD_HOSTING_REMOTE_ALLOW, and nothing of the admin side: admin API,
// dashboard, setup, sign-in and the browser terminal stay on loopback.

// remoteSettings is the remote listener: addr is "" when none is set.
type remoteSettings struct {
	addr  string
	allow []netip.Prefix
}

// hostingRemote reads the remote listener settings. The error names every
// reason the address is refused, joined by "; " as hostingStart's are.
func hostingRemote(getenv func(string) string) (remoteSettings, error) {
	addr, from := config.EnvFrom(getenv, config.HostingRemoteListenName)
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return remoteSettings{}, nil
	}
	allow, allowFrom, err := config.HostingRemoteAllow(getenv)
	if err != nil {
		return remoteSettings{}, err
	}
	host, port, err := net.SplitHostPort(addr)
	if n, perr := strconv.Atoi(port); err != nil || perr != nil || n < 1 || n > 65535 {
		return remoteSettings{}, fmt.Errorf("%s %q is not IP:port", from, addr)
	}
	ip, err := netip.ParseAddr(host)
	switch {
	case host == "" || err == nil && ip.IsUnspecified():
		return remoteSettings{}, errors.New(from + " " + addr + " binds every interface (give one private-network address)")
	case strings.EqualFold(host, "localhost") || err == nil && ip.IsLoopback():
		return remoteSettings{}, errors.New(from + " " + addr + " is a loopback address (LISTEN_ADDR already serves loopback)")
	case err != nil:
		return remoteSettings{}, errors.New(from + " " + addr + " names a host; give an IP address")
	case !config.AddrAllowed(ip, allow):
		return remoteSettings{}, errors.New(from + " " + addr + " is outside " + allowFrom + " (" + prefixList(allow) + ")")
	}
	return remoteSettings{addr: addr, allow: allow}, nil
}

func prefixList(allow []netip.Prefix) string {
	s := make([]string, len(allow))
	for i, p := range allow {
		s[i] = p.String()
	}
	return strings.Join(s, ",")
}

// remoteHandler is the remote listener's handler: a forwarded request or a
// peer outside allow is refused (403), and only the role API and the node
// channel are served; every other path is 404.
func (s *Server) remoteHandler(allow []netip.Prefix) http.Handler {
	inner := s.handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if forwardedRequest(r) {
			http.Error(w, "forwarded requests are refused on the remote listener", http.StatusForbidden)
			return
		}
		if !peerAllowed(r.RemoteAddr, allow) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if !remotePath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// forwardedRequest reports whether a proxy forwarding header is present: the
// remote listener is for direct connections only.
func forwardedRequest(r *http.Request) bool {
	if r.Header.Get("Forwarded") != "" || r.Header.Get("X-Real-Ip") != "" {
		return true
	}
	for k := range r.Header {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), "X-Forwarded-") {
			return true
		}
	}
	return false
}

func peerAllowed(remoteAddr string, allow []netip.Prefix) bool {
	ap, err := netip.ParseAddrPort(remoteAddr)
	return err == nil && config.AddrAllowed(ap.Addr(), allow)
}

// remotePath reports whether the remote listener serves path: /api/… except
// the admin API and the token-less role registration and invite join.
func remotePath(p string) bool {
	if path.Clean(p) != p || !strings.HasPrefix(p, "/api/") {
		return false
	}
	rest := strings.TrimPrefix(p, "/api/")
	switch {
	case rest == "admin" || strings.HasPrefix(rest, "admin/"):
		return false
	case rest == "register" || rest == "join":
		return false
	}
	return true
}

// nodeLookupTimeout bounds resolving a node's --host name.
const nodeLookupTimeout = 10 * time.Second

// checkNodeHost accepts an http(s) URL of a loopback host, or of a host whose
// every address is loopback or inside allow (SWITCHBOARD_HOSTING_REMOTE_ALLOW
// on the node). lookup resolves a host name.
func checkNodeHost(hostURL string, allow []netip.Prefix, lookup func(context.Context, string) ([]netip.Addr, error)) error {
	u, err := url.Parse(hostURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("node: --host %q is not an http:// or https:// URL", hostURL)
	}
	h := u.Hostname()
	if loopbackHost(h) {
		return nil
	}
	const name = config.EnvPrefix + config.HostingRemoteAllowName
	if ip, err := netip.ParseAddr(h); err == nil {
		if !ip.IsUnspecified() && config.AddrAllowed(ip, allow) {
			return nil
		}
		return fmt.Errorf("node: --host %s is neither loopback nor inside %s (%s); attach to the host's private-network listener (%s%s)", h, name, prefixList(allow), config.EnvPrefix, config.HostingRemoteListenName)
	}
	ctx, cancel := context.WithTimeout(context.Background(), nodeLookupTimeout)
	defer cancel()
	addrs, err := lookup(ctx, h)
	if err == nil && len(addrs) == 0 {
		err = errors.New("no address")
	}
	if err != nil {
		return fmt.Errorf("node: --host %s: cannot resolve: %v", h, err)
	}
	for _, a := range addrs {
		if !a.Unmap().IsLoopback() && !config.AddrAllowed(a, allow) {
			return fmt.Errorf("node: --host %s resolves to %s, outside %s (%s); attach to the host's private-network listener (%s%s)", h, a.Unmap(), name, prefixList(allow), config.EnvPrefix, config.HostingRemoteListenName)
		}
	}
	return nil
}
