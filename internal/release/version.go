package release

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a release tag read as semantic version 2.0.0: vMAJOR.MINOR.PATCH,
// an optional -prerelease and an ignored +build.
type Version struct {
	Major, Minor, Patch uint64
	Pre                 []string
}

// ParseVersion reads a release tag. The v prefix is required, as the tags
// carry it; a local build's "dev" is not a version.
func ParseVersion(s string) (Version, error) {
	bad := func(why string) (Version, error) {
		return Version{}, fmt.Errorf("version %q: %s", s, why)
	}
	rest, ok := strings.CutPrefix(s, "v")
	if !ok {
		return bad("want vMAJOR.MINOR.PATCH")
	}
	if i := strings.IndexByte(rest, '+'); i >= 0 {
		for _, id := range strings.Split(rest[i+1:], ".") {
			if !identifier(id) {
				return bad("bad build metadata")
			}
		}
		rest = rest[:i]
	}
	core, pre, hasPre := strings.Cut(rest, "-")
	nums := strings.Split(core, ".")
	if len(nums) != 3 {
		return bad("want vMAJOR.MINOR.PATCH")
	}
	var v Version
	for i, p := range []*uint64{&v.Major, &v.Minor, &v.Patch} {
		if !numeric(nums[i]) {
			return bad("want vMAJOR.MINOR.PATCH")
		}
		n, err := strconv.ParseUint(nums[i], 10, 64)
		if err != nil {
			return bad(err.Error())
		}
		*p = n
	}
	if hasPre {
		v.Pre = strings.Split(pre, ".")
		for _, id := range v.Pre {
			if !identifier(id) || (allDigits(id) && !numeric(id)) {
				return bad("bad prerelease")
			}
		}
	}
	return v, nil
}

// Compare orders versions by semver precedence: -1 when a < b, 0 when
// equal, +1 when a > b. A prerelease comes before its release; its
// identifiers compare numerically when both are numbers, else as text, and
// a number before text.
func Compare(a, b Version) int {
	for _, d := range [][2]uint64{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a.Pre) == 0 && len(b.Pre) == 0:
		return 0
	case len(a.Pre) == 0:
		return 1
	case len(b.Pre) == 0:
		return -1
	}
	for i := 0; i < len(a.Pre) && i < len(b.Pre); i++ {
		if c := compareIdent(a.Pre[i], b.Pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.Pre) < len(b.Pre):
		return -1
	case len(a.Pre) > len(b.Pre):
		return 1
	}
	return 0
}

func compareIdent(a, b string) int {
	an, bn := allDigits(a), allDigits(b)
	switch {
	case an && bn:
		// No leading zeros: the longer number is the larger.
		if len(a) != len(b) {
			if len(a) < len(b) {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	case an:
		return -1
	case bn:
		return 1
	}
	return strings.Compare(a, b)
}

// identifier is a non-empty run of [0-9A-Za-z-].
func identifier(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-') {
			return false
		}
	}
	return true
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// numeric is a number without leading zeros.
func numeric(s string) bool { return allDigits(s) && (s == "0" || s[0] != '0') }
