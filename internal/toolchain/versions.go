package toolchain

import (
	"fmt"
	"strconv"
	"strings"
)

// version is a release number such as 1.26.2 or 24.11.1. Pre-releases
// (1.27rc1) are not versions here: partial requests never pick them.
type version []int

func parseVersion(s string) (version, bool) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "go")
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ".")
	v := make(version, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		v = append(v, n)
	}
	return v, true
}

func (v version) String() string {
	s := make([]string, len(v))
	for i, n := range v {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ".")
}

// less compares versions component by component; a missing component
// counts as 0.
func (v version) less(w version) bool {
	for i := 0; i < len(v) || i < len(w); i++ {
		a, b := at(v, i), at(w, i)
		if a != b {
			return a < b
		}
	}
	return false
}

func at(v version, i int) int {
	if i < len(v) {
		return v[i]
	}
	return 0
}

// hasPrefix reports whether v starts with the components of p (24 matches
// 24.11.1; 1.26 matches 1.26 and 1.26.3).
func (v version) hasPrefix(p version) bool {
	if len(p) > len(v) {
		return false
	}
	for i := range p {
		if v[i] != p[i] {
			return false
		}
	}
	return true
}

// release is one version a provider offers.
type release struct {
	v   version
	lts string // Node.js LTS codename, lowercased
}

// match picks the newest release satisfying a request:
//
//   - "" or "latest": the newest release;
//   - "lts", "lts/*": the newest long-term-support release;
//   - "lts/<codename>": the newest release of that LTS line;
//   - "24", "1.26", "24.11.1": the newest release with those leading numbers;
//   - "24.x", "^22.1", "~22.1", ">=22": the npm-style ranges Node projects
//     use in engines.node (">=" prefers the newest LTS release).
func match(releases []release, request string) (version, error) {
	req := strings.TrimSpace(strings.ToLower(request))
	best := func(ok func(release) bool) version {
		var b version
		for _, r := range releases {
			if ok(r) && (b == nil || b.less(r.v)) {
				b = r.v
			}
		}
		return b
	}
	var v version
	switch {
	case req == "" || req == "latest" || req == "node" || req == "current" || req == "stable":
		v = best(func(release) bool { return true })
	case req == "lts" || req == "lts/*":
		v = best(func(r release) bool { return r.lts != "" })
	case strings.HasPrefix(req, "lts/"):
		name := strings.TrimPrefix(req, "lts/")
		v = best(func(r release) bool { return r.lts == name })
	case strings.HasPrefix(req, ">="):
		min, ok := parseVersion(strings.TrimSpace(req[2:]))
		if !ok {
			return nil, fmt.Errorf("cannot read the version request %q", request)
		}
		atLeast := func(r release) bool { return !r.v.less(min) }
		if v = best(func(r release) bool { return atLeast(r) && r.lts != "" }); v == nil {
			v = best(atLeast)
		}
	case strings.HasPrefix(req, "^") || strings.HasPrefix(req, "~"):
		min, ok := parseVersion(strings.TrimSpace(req[1:]))
		if !ok {
			return nil, fmt.Errorf("cannot read the version request %q", request)
		}
		keep := 1
		if req[0] == '~' && len(min) > 1 {
			keep = 2
		}
		v = best(func(r release) bool { return r.v.hasPrefix(min[:keep]) && !r.v.less(min) })
	default:
		req = strings.TrimSuffix(strings.TrimSuffix(req, ".x"), ".*")
		p, ok := parseVersion(req)
		if !ok {
			return nil, fmt.Errorf("cannot read the version request %q: give a version such as 24 or 1.26.2", request)
		}
		v = best(func(r release) bool { return r.v.hasPrefix(p) })
	}
	if v == nil {
		return nil, fmt.Errorf("no release matches %q", request)
	}
	return v, nil
}
