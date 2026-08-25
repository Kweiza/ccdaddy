// Command minisign-sign produces the minisign signature this repository
// publishes beside sha256sums.txt, and generates the keypair that signature is
// made with.
//
// It exists instead of a `minisign -S` line in the release workflow for one
// reason, and it is a flag: the stock tool signs PREHASHED unless it is given
// -l, and a prehashed signature is one every published ccdad refuses. Fixing
// the mode in internal/relsign removes the command line the flag could be
// dropped from. It also removes the need for minisign to be installed on the
// runner or on the maintainer's machine.
//
// It is deliberately NOT called from scripts/build-release.sh. Signing lives in
// the workflow so a developer can run that script locally with no secret and
// get the same six binaries and the same sums file, which is what
// scripts/build_release_test.go already asserts.
//
// Usage:
//
//	go run ./scripts/minisign-sign -G [-p ccdaddy.pub] [-s ccdaddy.key]
//	go run ./scripts/minisign-sign -m dist/sha256sums.txt -t v0.7.0 -o dist/sha256sums.txt.minisig
//
// In signing mode the secret key comes from MINISIGN_SECRET_KEY -- the second
// line of the file -G wrote -- unless -s names a file holding it. It is never
// an argument: a command line is readable from a process listing.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Kweiza/ccdaddy/internal/relsign"
)

const secretEnvVar = "MINISIGN_SECRET_KEY"

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "minisign-sign:", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string, stderr io.Writer) error {
	fs := flag.NewFlagSet("minisign-sign", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		gen = fs.Bool("G", false, "generate a keypair instead of signing")
		pub = fs.String("p", "ccdaddy.pub", "public key file: written by -G, read back for self-verification when signing")
		sec = fs.String("s", "", "secret key file (-G writes it; signing reads "+secretEnvVar+" when this is empty)")
		msg = fs.String("m", "", "file to sign")
		tag = fs.String("t", "", "release tag the signature is for, e.g. v0.7.0")
		out = fs.String("o", "", "signature file to write (default: the -m file plus .minisig)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *gen {
		return generateKeypair(*pub, *sec, stderr)
	}
	return sign(*msg, *tag, *out, *pub, *sec, getenv, stderr)
}

func generateKeypair(pubPath, secPath string, stderr io.Writer) error {
	if secPath == "" {
		secPath = "ccdaddy.key"
	}
	pubFile, secFile, err := relsign.GenerateKey()
	if err != nil {
		return err
	}
	// O_EXCL on both, and the public half first so a half-written pair leaves
	// the recoverable file behind rather than the unrecoverable one.
	// Overwriting a release key cannot be undone: its public half is committed
	// and its secret half exists in exactly one other place.
	if err := writeNew(pubPath, pubFile, 0o644); err != nil {
		return err
	}
	if err := writeNew(secPath, secFile, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "minisign-sign: wrote %s and %s\n", pubPath, secPath)
	fmt.Fprintf(stderr, "minisign-sign: commit %s; put the SECOND line of %s in the %s repository secret; never commit %s\n",
		pubPath, secPath, secretEnvVar, secPath)
	return nil
}

func writeNew(path, body string, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func sign(msgPath, tag, outPath, pubPath, secPath string, getenv func(string) string, stderr io.Writer) error {
	if msgPath == "" {
		return errors.New("-m is required")
	}
	// A tag without its leading v would produce a trusted comment naming
	// something no ccdad ever asks for, and the failure would surface as
	// "signature is for a different release" on every machine in the fleet.
	// Refusing here costs a release job; not refusing costs the release.
	if !strings.HasPrefix(tag, "v") || len(tag) < 2 || strings.ContainsAny(tag, " \t\r\n") {
		return fmt.Errorf("-t must be a release tag such as v0.7.0, got %q", tag)
	}
	if outPath == "" {
		outPath = msgPath + ".minisig"
	}
	body := getenv(secretEnvVar)
	if secPath != "" {
		raw, err := os.ReadFile(secPath)
		if err != nil {
			return fmt.Errorf("reading the secret key: %w", err)
		}
		lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
		body = lines[len(lines)-1]
	}
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("no secret key: set %s or pass -s", secretEnvVar)
	}
	sk, err := relsign.ParseSecretKey(body)
	if err != nil {
		return err
	}
	content, err := os.ReadFile(msgPath)
	if err != nil {
		return err
	}
	sig, err := sk.Sign(content, relsign.TrustedComment(tag))
	if err != nil {
		return err
	}

	// Self-verification through the SAME code the shipped binary runs, against
	// the public key committed in the tree rather than the one derived from the
	// secret. That is what makes this catch the failure worth catching: a
	// repository secret that is not the key ccdad carries. A signer nothing can
	// verify must fail the release job, not the user.
	key, err := readPublicKey(pubPath)
	if err != nil {
		return err
	}
	if err := relsign.Verify([]relsign.PublicKey{key}, content, sig, tag); err != nil {
		return fmt.Errorf("the signature just produced does not verify against %s: %w", pubPath, err)
	}

	if err := os.WriteFile(outPath, sig, 0o644); err != nil {
		// WriteFile uses O_TRUNC, truncating the file before writing. If the write
		// fails (disk full, I/O error), a truncated file is left at outPath. Clean
		// it up so the directory is restored to its state before the attempt.
		//
		// A process killed mid-write never reaches this line, so a truncated file
		// can be left behind. It is still not published — .github/workflows/release.yml
		// runs Build, Sign, Attest, and Publish as sequential steps with no
		// continue-on-error between them, so a step that dies takes the job with it
		// and the upload never runs. That protection is fragile: someone adding
		// if: always() to Publish or reordering steps would remove it without
		// touching this file.
		os.Remove(outPath)
		return err
	}
	fmt.Fprintf(stderr, "minisign-sign: signed %s as %s for %s\n", msgPath, outPath, tag)
	return nil
}

func readPublicKey(path string) (relsign.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return relsign.PublicKey{}, fmt.Errorf("reading the public key: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		return relsign.PublicKey{}, fmt.Errorf("%s has %d lines, want 2", path, len(lines))
	}
	return relsign.ParsePublicKey(lines[1])
}
