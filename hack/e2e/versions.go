package main

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// Release versions as the release workflow accepts them: 0.5.0 or
// 0.5.0-rc.1, with or without a leading v; no build metadata.

type semver struct {
	major, minor, patch int
	pre                 []string
}

func parseVersion(v string) (semver, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	core, pre, hasPre := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 || (hasPre && pre == "") {
		return semver{}, false
	}
	var nums [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" || (len(p) > 1 && p[0] == '0') {
			return semver{}, false
		}
		nums[i] = n
	}
	s := semver{major: nums[0], minor: nums[1], patch: nums[2]}
	if hasPre {
		for _, id := range strings.Split(pre, ".") {
			if id == "" || strings.Trim(id, "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-") != "" {
				return semver{}, false
			}
		}
		s.pre = strings.Split(pre, ".")
	}
	return s, true
}

func validVersion(v string) bool { _, ok := parseVersion(v); return ok }

// compareVersions orders by semantic version precedence; invalid versions
// sort first.
func compareVersions(a, b string) int {
	x, okx := parseVersion(a)
	y, oky := parseVersion(b)
	switch {
	case !okx && !oky:
		return 0
	case !okx:
		return -1
	case !oky:
		return 1
	}
	for _, d := range []int{x.major - y.major, x.minor - y.minor, x.patch - y.patch} {
		if d != 0 {
			return sign(d)
		}
	}
	switch {
	case len(x.pre) == 0 && len(y.pre) == 0:
		return 0
	case len(x.pre) == 0:
		return 1 // a release ranks above its prereleases
	case len(y.pre) == 0:
		return -1
	}
	for i := 0; i < len(x.pre) && i < len(y.pre); i++ {
		if c := comparePre(x.pre[i], y.pre[i]); c != 0 {
			return c
		}
	}
	return sign(len(x.pre) - len(y.pre))
}

func comparePre(a, b string) int {
	na, ea := strconv.Atoi(a)
	nb, eb := strconv.Atoi(b)
	switch {
	case ea == nil && eb == nil:
		return sign(na - nb)
	case ea == nil:
		return -1 // numeric identifiers rank below alphanumeric ones
	case eb == nil:
		return 1
	}
	return strings.Compare(a, b)
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// previousStable picks, from release tags (one per line, as `gh release
// list` prints them), the newest stable release older than target: the
// version an upgrade test starts from. "" when there is none.
func previousStable(tags io.Reader, target string) (string, error) {
	best := ""
	sc := bufio.NewScanner(tags)
	for sc.Scan() {
		tag := strings.TrimSpace(sc.Text())
		v, ok := parseVersion(tag)
		if !ok || len(v.pre) > 0 || compareVersions(tag, target) >= 0 {
			continue
		}
		if best == "" || compareVersions(tag, best) > 0 {
			best = tag
		}
	}
	return strings.TrimPrefix(best, "v"), sc.Err()
}

// latestStable is the newest stable release among tags.
func latestStable(tags io.Reader) (string, error) {
	best := ""
	sc := bufio.NewScanner(tags)
	for sc.Scan() {
		tag := strings.TrimSpace(sc.Text())
		v, ok := parseVersion(tag)
		if !ok || len(v.pre) > 0 {
			continue
		}
		if best == "" || compareVersions(tag, best) > 0 {
			best = tag
		}
	}
	return strings.TrimPrefix(best, "v"), sc.Err()
}
