// Package upgrades is what the Upgrade controller, the release discovery and
// `kwerft upgrade-runner` share (docs/phase6-upgrades.md): versions, the
// release manifests in the public install repository, maintenance windows,
// the installer's progress lines, and the runner's state machine.
package upgrades

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a Kwerft release (0.6.0, 0.6.0-rc.1) or a k3s version
// (v1.38.1+k3s1). Unlike semver, k3s's build suffix orders versions:
// v1.38.1+k3s2 is newer than v1.38.1+k3s1.
type Version struct {
	Major, Minor, Patch int
	Pre                 string // without the dash
	Build               string // without the plus
	raw                 string
}

// ParseVersion reads major.minor.patch with an optional "v", pre-release and
// build.
func ParseVersion(s string) (Version, error) {
	v := Version{raw: s}
	rest := strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(rest, '+'); i >= 0 {
		v.Build, rest = rest[i+1:], rest[:i]
	}
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		v.Pre, rest = rest[i+1:], rest[:i]
		if v.Pre == "" {
			return Version{}, fmt.Errorf("version %q: empty pre-release", s)
		}
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("version %q: want major.minor.patch", s)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (len(p) > 1 && p[0] == '0') {
			return Version{}, fmt.Errorf("version %q: %q is not a number", s, p)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, nil
}

// MustVersion is ParseVersion for constants (tests).
func MustVersion(s string) Version {
	v, err := ParseVersion(s)
	if err != nil {
		panic(err)
	}
	return v
}

func (v Version) String() string { return v.raw }

// MinorString is "1.38" (what kubernetes.supported lists).
func (v Version) MinorString() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// SameMinor: same major and minor.
func (v Version) SameMinor(o Version) bool { return v.Major == o.Major && v.Minor == o.Minor }

// MinorsAhead is how many minor releases v is ahead of o (negative when
// behind); a major step counts as many.
func (v Version) MinorsAhead(o Version) int {
	return (v.Major-o.Major)*1000 + v.Minor - o.Minor
}

// Compare returns -1, 0 or 1.
func (v Version) Compare(o Version) int {
	for _, d := range []int{v.Major - o.Major, v.Minor - o.Minor, v.Patch - o.Patch} {
		if d != 0 {
			return sign(d)
		}
	}
	if c := comparePre(v.Pre, o.Pre); c != 0 {
		return c
	}
	return compareBuild(v.Build, o.Build)
}

func (v Version) Less(o Version) bool { return v.Compare(o) < 0 }

func sign(d int) int {
	switch {
	case d < 0:
		return -1
	case d > 0:
		return 1
	}
	return 0
}

// comparePre orders pre-releases as semver does: none is newer than any.
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
		if c := compareIdent(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return sign(len(as) - len(bs))
}

func compareIdent(a, b string) int {
	an, aerr := strconv.Atoi(a)
	bn, berr := strconv.Atoi(b)
	switch {
	case aerr == nil && berr == nil:
		return sign(an - bn)
	case aerr == nil:
		return -1 // numeric identifiers are older than alphanumeric ones
	case berr == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// compareBuild orders k3s's "k3s1" < "k3s2" by the trailing number.
func compareBuild(a, b string) int {
	if a == b {
		return 0
	}
	ap, an := splitTrailingNumber(a)
	bp, bn := splitTrailingNumber(b)
	if ap == bp && an >= 0 && bn >= 0 {
		return sign(an - bn)
	}
	return strings.Compare(a, b)
}

func splitTrailingNumber(s string) (string, int) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == len(s) {
		return s, -1
	}
	n, err := strconv.Atoi(s[i:])
	if err != nil {
		return s, -1
	}
	return s[:i], n
}

// IsRelease reports whether v is a published Kwerft release version: no
// development suffix (the default 0.1.0-dev, git describe output).
func IsRelease(v string) bool {
	pv, err := ParseVersion(v)
	if err != nil || pv.Build != "" {
		return false
	}
	return !strings.HasSuffix(pv.Pre, "dev") && !strings.Contains(v, "-g") && !strings.Contains(v, "dirty")
}

// UpgradeKind is Patch within a minor, Minor otherwise.
func UpgradeKind(from, to Version) string {
	if from.SameMinor(to) {
		return "Patch"
	}
	return "Minor"
}
