package presence

import (
	"net/http"
	"sort"
	"strings"
	"unicode"

	"github.com/sandcorporation/switchboard/internal/config"
	"github.com/sandcorporation/switchboard/internal/identity"
	"github.com/sandcorporation/switchboard/internal/wire"
)

// CLI versions on slots (#308): see wire.CLIState.

// maxCLIVersion bounds the recorded header value.
const maxCLIVersion = 64

// cliVersionHeader is the request's X-Switchboard-Version, cleaned:
// printable, at most maxCLIVersion bytes; empty when absent.
func cliVersionHeader(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get(config.HeaderPrefix + config.VersionHeaderName))
	v = strings.Map(func(c rune) rune {
		if c > unicode.MaxASCII || !unicode.IsPrint(c) || c == ' ' {
			return -1
		}
		return c
	}, v)
	if len(v) > maxCLIVersion {
		v = v[:maxCLIVersion]
	}
	return v
}

// cliState is version against the configured minimum.
func (s *Service) cliState(version string) string {
	if s.cfg == nil {
		return wire.CLIState(version, wire.MinorVersion{}, false)
	}
	return wire.CLIState(version, s.cfg.CLIMin, s.cfg.CLIMinOK)
}

// cliBehind reports whether version needs an update notice: a release below
// the minimum. Unknown (no header) and a development build are 판 모름, not
// 옛 판 (#319), and nothing is behind on a server without a minimum (itself a
// development build).
func (s *Service) cliBehind(version string) bool {
	if s.cfg == nil || !s.cfg.CLIMinOK {
		return false
	}
	return s.cliState(version) == wire.CLIOld
}

// CLIBehind lists the machines whose online sessions run an old CLI, one
// entry per machine, sorted by machine. Slots that have not
// reported since #308 are left out.
func (s *Service) CLIBehind() ([]wire.CLIBehind, error) {
	rows, err := s.db.Query(`SELECT base,number,machine,cli_version FROM slots WHERE lease_until>? AND cli_version<>'' ORDER BY machine,base,number`, ms(s.now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	by := map[string]*wire.CLIBehind{}
	for rows.Next() {
		var base, machine, version string
		var number int
		if err := rows.Scan(&base, &number, &machine, &version); err != nil {
			return nil, err
		}
		if !s.cliBehind(version) {
			continue
		}
		b := by[machine]
		if b == nil {
			b = &wire.CLIBehind{Machine: machine, Version: version}
			by[machine] = b
		}
		b.Roles = append(b.Roles, identity.SlotRole(base, number))
		b.RoleVersions = append(b.RoleVersions, version)
		b.Version = lowerCLI(b.Version, version)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]wire.CLIBehind, 0, len(by))
	for _, b := range by {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Machine < out[j].Machine })
	return out, nil
}

// lowerCLI is the lower of two behind (release) versions.
func lowerCLI(a, b string) string {
	va, _ := wire.ReleaseMinor(a)
	vb, _ := wire.ReleaseMinor(b)
	if vb.Less(va) {
		return b
	}
	return a
}
