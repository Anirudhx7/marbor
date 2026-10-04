package bootstrapcred

import (
	"fmt"
	"strings"
)

// parseICACLS reads the output of `icacls <path>` and returns the principal of
// every access entry plus whether any entry is inherited. The listing is the
// path followed by the first entry on one line, further entries on indented
// lines, then a blank line and a summary that is ignored. Each entry looks like
// `DOMAIN\name:(F)`; a principal may contain spaces. Output that has no entry at
// all is an error, so an unrecognised format fails closed instead of passing.
func parseICACLS(out, path string) (principals []string, inherited bool, err error) {
	started := false
	for i, line := range strings.Split(out, "\n") {
		entry := strings.TrimSpace(line)
		if entry == "" {
			if started {
				break
			}
			continue
		}
		if i == 0 || !started {
			if len(entry) >= len(path) && strings.EqualFold(entry[:len(path)], path) {
				entry = strings.TrimSpace(entry[len(path):])
			}
		}
		idx := strings.Index(entry, ":(")
		if idx <= 0 {
			if started {
				break
			}
			continue
		}
		started = true
		principals = append(principals, entry[:idx])
		if strings.Contains(entry[idx:], "(I)") {
			inherited = true
		}
	}
	if len(principals) == 0 {
		return nil, false, fmt.Errorf("the access list of %s could not be read", path)
	}
	return principals, inherited, nil
}

// aclAllowsOnly reports a problem unless every entry belongs to the given
// principal (its account name or SID, compared case-insensitively), or to the
// built-in SYSTEM or Administrators principals (see trustedWindowsPrincipals),
// and none is inherited. It returns "" when the list is acceptable.
func aclAllowsOnly(principals []string, inherited bool, account, sid string) string {
	if inherited {
		return "it inherits permissions from its folder"
	}
	for _, p := range principals {
		if !strings.EqualFold(p, account) && !strings.EqualFold(p, sid) && !isTrustedWindowsPrincipal(p) {
			return fmt.Sprintf("it grants access to %q", p)
		}
	}
	return ""
}

// trustedWindowsPrincipals are the built-in principals tolerated beside the
// service account: they can already read or take ownership of any local file, so
// refusing them would only lock out an ordinary install. Both the English names
// and the well-known SIDs are listed because icacls may print either.
var trustedWindowsPrincipals = []string{
	`NT AUTHORITY\SYSTEM`, `BUILTIN\Administrators`, "S-1-5-18", "S-1-5-32-544",
}

func isTrustedWindowsPrincipal(p string) bool {
	for _, t := range trustedWindowsPrincipals {
		if strings.EqualFold(p, t) {
			return true
		}
	}
	return false
}
