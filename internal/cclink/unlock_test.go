package cclink

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// THE PASSWORD MUST NEVER REACH ccdad. The whole design of this call is that
// `security` does its own asking on the terminal, so the secret goes from the
// user's fingers into Apple's binary and ccdad sees an exit code. The argument
// vector is where that would be given away: `-p <password>` is the
// non-interactive form, and it is the one thing this must never grow.
func TestUnlockLoginKeychainNeverPassesAPassword(t *testing.T) {
	argv := fakeSecurity{exit: 0}.install(t)

	if err := UnlockLoginKeychain(context.Background()); err != nil {
		t.Fatalf("UnlockLoginKeychain: %v", err)
	}
	got := recordedArgv(t, argv)
	if len(got) != 1 || got[0] != "unlock-keychain" {
		t.Fatalf("argv = %q, want exactly [unlock-keychain]: anything else is an argument ccdad chose "+
			"to put on a command line that asks for the login keychain's password", got)
	}
	for _, arg := range got {
		if arg == "-p" || strings.HasPrefix(arg, "-p") {
			t.Fatalf("argv carries %q, which is the non-interactive password form: %q", arg, got)
		}
	}
}

// A non-zero exit travels. The caller decides what to do about a refusal or a
// cancelled prompt; this reports it and nothing else.
func TestUnlockLoginKeychainReportsARefusal(t *testing.T) {
	fakeSecurity{exit: 36}.install(t)
	if err := UnlockLoginKeychain(context.Background()); err == nil {
		t.Fatal("a refused unlock reported success")
	}
}

// Off macOS there is no keychain to unlock, and nothing is spawned. The absence
// of the argv file is the proof, the same way TestRunSecurityRefusesOffDarwin
// makes it.
func TestUnlockLoginKeychainRefusesOffDarwin(t *testing.T) {
	argv := fakeSecurity{}.install(t)
	keychainGOOS = "linux"

	if err := UnlockLoginKeychain(context.Background()); !errors.Is(err, ErrKeychainUnsupported) {
		t.Fatalf("err = %v, want ErrKeychainUnsupported", err)
	}
	if _, statErr := os.Stat(argv); statErr == nil {
		t.Fatal("a security process was started on a platform with no keychain")
	}
}
