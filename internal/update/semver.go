package update

import (
	"regexp"
	"strconv"
	"strings"
)

// ver is a parsed version: a release (0.3.0), a pre-release (0.4.0-rc.1)
// or a build between releases as `git describe` prints it
// (0.3.0-4-gabc1234[-dirty]: 4 commits after 0.3.0).
type ver struct {
	ok    bool // false: not a version at all ("dev", "dev-abc1234")
	n     [3]int
	pre   string
	ahead int
	dirty bool
}

var (
	coreRe     = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?$`)
	describeRe = regexp.MustCompile(`^(.*)-(\d+)-g[0-9a-f]+$`)
)

func parseVersion(s string) ver {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	var v ver
	if t, ok := strings.CutSuffix(s, "-dirty"); ok {
		s, v.dirty = t, true
	}
	if m := describeRe.FindStringSubmatch(s); m != nil {
		s = m[1]
		v.ahead, _ = strconv.Atoi(m[2])
	}
	m := coreRe.FindStringSubmatch(s)
	if m == nil {
		return ver{}
	}
	for i := range 3 {
		v.n[i], _ = strconv.Atoi(m[i+1])
	}
	v.pre, v.ok = m[4], true
	return v
}

// release reports whether v is exactly a published version (not a local
// build between releases or with uncommitted changes).
func (v ver) release() bool { return v.ok && v.ahead == 0 && !v.dirty }

// newer reports whether version a is newer than b. Anything is newer than
// an unknown version (e.g. a "dev" build), so such builds are offered the
// latest release.
func newer(a, b string) bool { return compare(parseVersion(a), parseVersion(b)) > 0 }

func compare(a, b ver) int {
	switch {
	case !a.ok && !b.ok:
		return 0
	case !a.ok:
		return -1
	case !b.ok:
		return 1
	}
	for i := range 3 {
		if c := cmpInt(a.n[i], b.n[i]); c != 0 {
			return c
		}
	}
	if c := comparePre(a.pre, b.pre); c != 0 {
		return c
	}
	if c := cmpInt(a.ahead, b.ahead); c != 0 {
		return c
	}
	// Uncommitted changes on top of the same commit count as later.
	switch {
	case a.dirty && !b.dirty:
		return 1
	case !a.dirty && b.dirty:
		return -1
	}
	return 0
}

// comparePre orders pre-release suffixes the semver way: no suffix (the
// release) is newest; dot-separated parts compare numerically when both
// are numbers, otherwise as text.
func comparePre(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, xerr := strconv.Atoi(as[i])
		y, yerr := strconv.Atoi(bs[i])
		var c int
		switch {
		case xerr == nil && yerr == nil:
			c = cmpInt(x, y)
		case xerr == nil:
			c = -1 // numbers sort before words
		case yerr == nil:
			c = 1
		default:
			c = strings.Compare(as[i], bs[i])
		}
		if c != 0 {
			return c
		}
	}
	return cmpInt(len(as), len(bs))
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
