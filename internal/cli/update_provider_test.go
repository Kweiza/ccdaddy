package cli

import (
	"strings"
	"testing"
)

// `ccdad update --version <older tag>` with Codex accounts in the store warns
// and proceeds.
//
// It proceeds because --version IS the consent: naming a tag is already the
// deliberate act this command asks for, and a second --force would be a second
// gate on a road the user has said twice they want to go down. It warns
// because the consequence is not obvious from the tag: a build that does not
// know the provider field reads the document, drops the key, and writes it
// back -- and the store then refuses to open for the build the user upgrades
// back to.
func TestUpdateWarnsWhenPinningAnOlderBuildWithCodexAccounts(t *testing.T) {
	// updateWorld is this file's own harness: it isolates, stubs the version,
	// the install target, the release origin and the daemon, so the command
	// gets past its six preflight refusals and reaches the version step. It
	// calls isolate itself, so the accounts are seeded after it.
	updateWorld(t, "v0.9.10", "v0.9.10")
	seedAccount(t, "u-1", "a@example.com")
	seedCodexAccount(t, "u-x", "c@example.com")

	// The download of a tag the fake origin does not serve fails afterwards.
	// That is not what is under test: the warning is printed before anything
	// is fetched, which is the whole point of where it sits.
	_, _, errOut, _ := runRoot(t, "update", "--version", "v0.0.1")
	if !strings.Contains(errOut, "Codex") {
		t.Fatalf("update --version said nothing about the Codex accounts:\n%s", errOut)
	}
	if !strings.Contains(errOut, "accounts.toml") {
		t.Errorf("the warning does not name what would be rewritten:\n%s", errOut)
	}
}

// No Codex account, no warning. A line about a risk that does not exist on
// this machine is noise, and this command already prints a great deal.
func TestUpdateSaysNothingWithNoCodexAccounts(t *testing.T) {
	updateWorld(t, "v0.9.10", "v0.9.10")
	seedAccount(t, "u-1", "a@example.com")

	_, _, errOut, _ := runRoot(t, "update", "--version", "v0.0.1")
	if strings.Contains(errOut, "Codex") {
		t.Fatalf("update --version warned about Codex on a store with none:\n%s", errOut)
	}
}

// An unpinned update says nothing either: it is going forward, and the
// document a newer build writes is one it can read.
func TestUpdateSaysNothingWithoutAPinnedVersion(t *testing.T) {
	updateWorld(t, "v0.9.10", "v0.9.10")
	seedCodexAccount(t, "u-x", "c@example.com")

	_, _, errOut, _ := runRoot(t, "update", "--check")
	if strings.Contains(errOut, "Codex") {
		t.Fatalf("an unpinned update warned about Codex:\n%s", errOut)
	}
}

// Pinning a NEWER build says nothing either: the document a newer build writes
// is one it can read, and this warning is about going BACK. A warning on every
// pinned tag would fire on the ordinary `ccdad update --version <next release>`
// and teach a user to scroll past it.
func TestUpdateSaysNothingWhenPinningANewerBuild(t *testing.T) {
	updateWorld(t, "v0.9.10", "v0.9.10")
	seedCodexAccount(t, "u-x", "c@example.com")

	_, _, errOut, _ := runRoot(t, "update", "--version", "v9.9.9")
	if strings.Contains(errOut, "Codex") {
		t.Fatalf("pinning a newer build warned about Codex:\n%s", errOut)
	}
}
