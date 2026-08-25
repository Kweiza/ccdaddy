package main

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/relsign"
)

func noEnv(string) string { return "" }

// generate runs -G into a temp directory and hands back the two paths.
func generate(t *testing.T) (dir, pub, sec string) {
	t.Helper()
	dir = t.TempDir()
	pub = filepath.Join(dir, "ccdaddy.pub")
	sec = filepath.Join(dir, "ccdaddy.key")
	if err := run([]string{"-G", "-p", pub, "-s", sec}, noEnv, io.Discard); err != nil {
		t.Fatalf("-G: %v", err)
	}
	return dir, pub, sec
}

func TestGenerateWritesBothFilesAndRefusesToClobber(t *testing.T) {
	_, pub, sec := generate(t)

	for _, p := range []string{pub, sec} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if runtime.GOOS != "windows" {
		// A mode is not an ACL, so this asks the question only where the answer
		// means something.
		info, err := os.Stat(sec)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("secret key mode = %04o, want 0600", perm)
		}
	}
	// Overwriting the release key is unrecoverable: the public half is
	// committed and the secret half exists in exactly one other place.
	if err := run([]string{"-G", "-p", pub, "-s", sec}, noEnv, io.Discard); err == nil {
		t.Fatal("-G overwrote an existing keypair")
	}
}

// The refusal this test pins is what keeps a secret key from ever landing at
// the repository root: -G must fail before it writes anything, not merely
// write somewhere safer by default.
func TestGenerateRefusesWithoutASecretPath(t *testing.T) {
	err := run([]string{"-G"}, noEnv, io.Discard)
	if err == nil {
		t.Fatal("-G with no -s did not fail")
	}
	if !strings.Contains(err.Error(), "-s") {
		t.Fatalf("error = %v, want it to mention -s", err)
	}
}

func TestSignModeRoundTripsAndSelfVerifies(t *testing.T) {
	dir, pub, sec := generate(t)
	msg := filepath.Join(dir, "sha256sums.txt")
	if err := os.WriteFile(msg, []byte("0123456789abcdef  ccdad-linux-amd64\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := msg + ".minisig"

	if err := run([]string{"-m", msg, "-t", "v1.2.3", "-o", out, "-p", pub, "-s", sec}, noEnv, io.Discard); err != nil {
		t.Fatalf("sign: %v", err)
	}

	content, err := os.ReadFile(msg)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	pubLine := strings.Split(strings.TrimRight(readFile(t, pub), "\n"), "\n")[1]
	key, err := relsign.ParsePublicKey(pubLine)
	if err != nil {
		t.Fatal(err)
	}
	if err := relsign.Verify([]relsign.PublicKey{key}, content, sig, "v1.2.3"); err != nil {
		t.Fatalf("the signature this tool wrote does not verify: %v", err)
	}
}

func TestSignModeReadsTheSecretFromTheEnvironment(t *testing.T) {
	dir, pub, sec := generate(t)
	msg := filepath.Join(dir, "sha256sums.txt")
	if err := os.WriteFile(msg, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := strings.Split(strings.TrimRight(readFile(t, sec), "\n"), "\n")[1]
	env := func(k string) string {
		if k == "MINISIGN_SECRET_KEY" {
			return body + "\n" // a secret store round-trips a trailing newline
		}
		return ""
	}
	if err := run([]string{"-m", msg, "-t", "v1.2.3", "-o", msg + ".minisig", "-p", pub}, env, io.Discard); err != nil {
		t.Fatalf("sign from the environment: %v", err)
	}
}

// The self-verification is the step that catches "the secret in the repository
// settings is not the key committed in the tree" -- which is otherwise found by
// users, one failed update at a time.
func TestSignModeRefusesAKeyThatIsNotTheCommittedOne(t *testing.T) {
	dir, _, sec := generate(t)
	_, otherPub, _ := generate(t)
	msg := filepath.Join(dir, "sha256sums.txt")
	if err := os.WriteFile(msg, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"-m", msg, "-t", "v1.2.3", "-o", msg + ".minisig", "-p", otherPub, "-s", sec}, noEnv, io.Discard)
	if err == nil {
		t.Fatal("signed with a key the given public key does not match")
	}
	if !strings.Contains(err.Error(), "does not verify") {
		t.Fatalf("error = %v, want it to say the signature does not verify against the given public key", err)
	}
	if _, statErr := os.Stat(msg + ".minisig"); statErr == nil {
		t.Error("a signature that failed self-verification was written anyway")
	}
}

func TestSignModeRefusesATagWithoutALeadingV(t *testing.T) {
	dir, pub, sec := generate(t)
	msg := filepath.Join(dir, "sha256sums.txt")
	if err := os.WriteFile(msg, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"", "1.2.3", "v1.2.3\tx", "v 1.2.3"} {
		if err := run([]string{"-m", msg, "-t", tag, "-o", msg + ".minisig", "-p", pub, "-s", sec}, noEnv, io.Discard); err == nil {
			t.Errorf("accepted the tag %q", tag)
		}
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
