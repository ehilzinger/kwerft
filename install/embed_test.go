// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package install

import (
	"strings"
	"testing"
)

func TestInstallerStamping(t *testing.T) {
	if !strings.Contains(Installer("v0.4.2"), "\nKWERFT_VERSION_DEFAULT=\"0.4.2\"\n") {
		t.Fatal("release version not stamped")
	}
	if Installer("dev-abc123") != installer {
		t.Fatal("development builds must serve the installer as is")
	}
	j := Join("https://ops.example.com", ReleaseURL("0.4.2"))
	if strings.Contains(j, "__CONSOLE_URL__") || !strings.Contains(j, `"${KWERFT_CONSOLE_URL:-https://ops.example.com}"`) ||
		!strings.Contains(j, `INSTALLER_URL="https://kwerft.dev/v0.4.2/install.sh"`) {
		t.Fatalf("join.sh:\n%s", j)
	}
	if ReleaseURL("0.1.0-dev") != "" || ReleaseURL("abc") != "" {
		t.Fatal("no release URL for development builds")
	}
}
