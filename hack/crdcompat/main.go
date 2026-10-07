// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Command crdcompat checks that the chart's CRDs stay compatible with those
// of an earlier release, so a rollback to that release keeps working: an
// upgrade rolls back the Helm releases but never the CRDs
// (docs/phase6-upgrades.md, Compatibility rules). Within a served version a
// schema may only gain optional fields: no removed or renamed fields, no new
// required fields, no narrowed enums, types or bounds.
//
//	go run ./hack/crdcompat OLD_DIR NEW_DIR
//	hack/release.sh crdcompat 0.6.0    # against the previous release tags
//
// Exit status: 0 compatible, 1 incompatible (each finding on stdout),
// 2 usage or read errors. The release workflow runs it through
// `hack/release.sh crdcompat`, which lets a tag with the trailer
// `Rollback-Safe: no` pass anyway.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("crdcompat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "what to call OLD_DIR in messages, such as v0.5.0 (default: the path)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: crdcompat [-name NAME] OLD_DIR NEW_DIR")
		fmt.Fprintln(stderr, "Fails when the CRDs in NEW_DIR are not a compatible extension of those in OLD_DIR.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	oldDir, newDir := fs.Arg(0), fs.Arg(1)
	label := oldDir
	if *name != "" {
		label = *name
	}
	oldCRDs, err := LoadDir(oldDir)
	if err != nil {
		fmt.Fprintf(stderr, "crdcompat: %s: %v\n", oldDir, err)
		return 2
	}
	newCRDs, err := LoadDir(newDir)
	if err != nil {
		fmt.Fprintf(stderr, "crdcompat: %s: %v\n", newDir, err)
		return 2
	}
	if len(newCRDs) == 0 {
		fmt.Fprintf(stderr, "crdcompat: no CRDs in %s\n", newDir)
		return 2
	}
	findings := Compare(oldCRDs, newCRDs)
	incompatible, warnings := 0, 0
	for _, f := range findings {
		fmt.Fprintln(stdout, f)
		if f.Warning {
			warnings++
		} else {
			incompatible++
		}
	}
	if incompatible > 0 {
		fmt.Fprintf(stderr, "crdcompat: %d incompatible change(s), %d warning(s) since %s\n", incompatible, warnings, label)
		return 1
	}
	fmt.Fprintf(stderr, "crdcompat: %d CRDs compatible with the %d in %s (%d warning(s))\n", len(newCRDs), len(oldCRDs), label, warnings)
	return 0
}
