// Package update provides semver comparison helpers.
package update

import (
	"strconv"
	"strings"
)

// IsNewer reports whether latest is a higher version than current.
// Both strings may have an optional "v" prefix. Returns false when
// either side is empty or current is "dev".
func IsNewer(latest, current string) bool {
	a := strings.TrimPrefix(strings.TrimSpace(latest), "v")
	b := strings.TrimPrefix(strings.TrimSpace(current), "v")
	if a == "" || b == "" || b == "dev" || a == b {
		return false
	}
	as := strings.Split(stripPre(a), ".")
	bs := strings.Split(stripPre(b), ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		ai, bi := 0, 0
		if i < len(as) {
			ai, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bi, _ = strconv.Atoi(bs[i])
		}
		if ai > bi {
			return true
		}
		if ai < bi {
			return false
		}
	}
	return false
}

func stripPre(v string) string {
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		return v[:i]
	}
	return v
}
