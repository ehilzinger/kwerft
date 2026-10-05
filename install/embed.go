// Package install embeds the installer and join.sh, which the console
// serves at /install.sh and /join.sh: a node joining a cluster runs the
// installer of the console's own version (the same k3s and Cilium pins).
package install

import (
	_ "embed"
	"regexp"
	"strings"
)

//go:embed install.sh
var installer string

//go:embed join.sh
var joinScript string

var semver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// Installer returns install.sh stamped with version (as hack/release.sh
// does) when it is a release version; development builds keep the default.
func Installer(version string) string {
	v := strings.TrimPrefix(version, "v")
	if !semver.MatchString(v) {
		return installer
	}
	return regexp.MustCompile(`(?m)^KWERFT_VERSION_DEFAULT="[^"]*"`).ReplaceAllLiteralString(installer, `KWERFT_VERSION_DEFAULT="`+v+`"`)
}

// Join returns join.sh for a console at consoleURL (https://host). The
// release's installer on GitHub is its fallback when installerURL is set.
func Join(consoleURL, installerURL string) string {
	s := strings.ReplaceAll(joinScript, "__CONSOLE_URL__", consoleURL)
	if installerURL != "" {
		s = regexp.MustCompile(`(?m)^INSTALLER_URL="[^"]*"`).ReplaceAllLiteralString(s, `INSTALLER_URL="`+installerURL+`"`)
	}
	return s
}

// ReleaseURL is where a release's installer lives in the public install
// repository (hack/release.sh installer_url), or "" for development builds.
func ReleaseURL(version string) string {
	v := strings.TrimPrefix(version, "v")
	if !semver.MatchString(v) || strings.HasSuffix(v, "-dev") {
		return ""
	}
	return "https://raw.githubusercontent.com/ehilzinger/kwerft-install/main/v" + v + "/install.sh"
}
