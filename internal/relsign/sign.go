package relsign

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
)

// A secret key body is alg(2) || key id(8) || ed25519 private key(64).
const secKeyLen = 2 + 8 + ed25519.PrivateKeySize

// SecretKey is the signing half of a release key.
//
// The PUBLIC key file this repository commits is minisign's exact format,
// because `minisign -Vm -p ccdaddy.pub` is how a human checks a download. The
// SECRET key file deliberately is not, and that is a decision rather than an
// omission:
//
//   - minisign's secret-key layout ends in a BLAKE2b-256 checksum, and BLAKE2b
//     is not in the standard library. Writing that field would need a module
//     this go.mod does not have; writing zeros in its place produces a file the
//     stock tool refuses with a message about a wrong password, which is a
//     worse failure than a file it plainly does not recognise.
//   - The stock `minisign -S` must never be pointed at this key anyway: it
//     signs PREHASHED unless it is given -l, and a prehashed signature is one
//     every published ccdad refuses. A secret key the stock tool cannot open is
//     therefore a guard, not a gap.
//
// The checksum is not merely unwritable, it is also unnecessary here: what it
// protects against is a key corrupted on disk, and scripts/minisign-sign
// verifies every signature it produces against the committed public key before
// the release job is allowed to continue.
type SecretKey struct {
	KeyNum [8]byte
	Key    ed25519.PrivateKey
}

// ParseSecretKey parses the base64 body of a ccdaddy release secret key -- the
// second line of the file `scripts/minisign-sign -G` writes, and the exact
// string the MINISIGN_SECRET_KEY repository secret holds.
//
// Whitespace is trimmed because a secret store round-trips a trailing newline
// more often than not, and a release job failing on an invisible byte is a
// failure nobody can read.
func ParseSecretKey(b64Body string) (SecretKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64Body))
	if err != nil {
		return SecretKey{}, fmt.Errorf("%w: secret key is not base64: %v", ErrMalformed, err)
	}
	if len(raw) != secKeyLen {
		return SecretKey{}, fmt.Errorf("%w: secret key is %d bytes, want %d", ErrMalformed, len(raw), secKeyLen)
	}
	if string(raw[0:2]) != algLegacy {
		return SecretKey{}, fmt.Errorf("%w: secret key algorithm %q", ErrAlgorithm, raw[0:2])
	}
	var sk SecretKey
	copy(sk.KeyNum[:], raw[2:10])
	sk.Key = ed25519.PrivateKey(bytes.Clone(raw[10:secKeyLen]))
	return sk, nil
}

// keyIDHex renders an 8-byte key id the way the stock tool prints it: the bytes
// read as a little-endian 64-bit integer, uppercase. Nothing parses this -- it
// appears only in untrusted comments, which are signed by nothing -- but a
// maintainer comparing a key file against a signature reads it, and two
// spellings of one id would be a diagnostic that lies.
func keyIDHex(n [8]byte) string {
	return fmt.Sprintf("%016X", binary.LittleEndian.Uint64(n[:]))
}

// Sign produces the four-line .minisig body for content.
//
// The algorithm is fixed here rather than chosen by a caller, and there is no
// parameter for it: a parameter is a thing a workflow gets wrong once and then
// publishes six binaries nobody can verify. Legacy "Ed" is a plain ed25519
// signature over the file's own bytes, which is what every verifier this
// repository ships understands.
func (s SecretKey) Sign(content []byte, trustedComment string) ([]byte, error) {
	if strings.ContainsAny(trustedComment, "\r\n") {
		return nil, fmt.Errorf("%w: the trusted comment contains a line break", ErrMalformed)
	}
	if len(s.Key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: secret key is %d bytes, want %d", ErrMalformed, len(s.Key), ed25519.PrivateKeySize)
	}
	sig := ed25519.Sign(s.Key, content)

	body := make([]byte, 0, sigLen)
	body = append(body, algLegacy...)
	body = append(body, s.KeyNum[:]...)
	body = append(body, sig...)

	// The global signature covers the signature AND the trusted comment, in
	// that order and with no separator. Signing the comment alone would leave
	// the two halves swappable between releases.
	signed := make([]byte, 0, len(sig)+len(trustedComment))
	signed = append(signed, sig...)
	signed = append(signed, trustedComment...)
	global := ed25519.Sign(s.Key, signed)

	var b strings.Builder
	b.WriteString(untrustedPrefix + "signature from ccdaddy release key " + keyIDHex(s.KeyNum) + "\n")
	b.WriteString(base64.StdEncoding.EncodeToString(body) + "\n")
	b.WriteString(trustedPrefix + trustedComment + "\n")
	b.WriteString(base64.StdEncoding.EncodeToString(global) + "\n")
	return []byte(b.String()), nil
}

// GenerateKey returns the two full file bodies for a fresh release keypair: the
// public key in minisign's own format, and the secret key in this repository's
// (SecretKey's comment explains why they differ).
//
// It exists because the maintainer machine does not have minisign installed,
// and requiring a package manager to bootstrap this repository's own key would
// put a third party between the repository and its trust root.
//
// NO PASSWORD. A password stored in the same secret store as the key it
// protects protects against nothing, and it adds a stdin prompt that hangs on
// the non-tty a release job runs on.
func GenerateKey() (pubFile, secFile string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	// ONE key id, used by both halves. Two draws would produce a pair whose
	// signatures the pair's own public key rejects with ErrKeyID -- a failure
	// that looks exactly like tampering and would be discovered at a release.
	var keyNum [8]byte
	if _, err := rand.Read(keyNum[:]); err != nil {
		return "", "", err
	}

	pubBody := make([]byte, 0, pubKeyLen)
	pubBody = append(pubBody, algLegacy...)
	pubBody = append(pubBody, keyNum[:]...)
	pubBody = append(pubBody, pub...)

	secBody := make([]byte, 0, secKeyLen)
	secBody = append(secBody, algLegacy...)
	secBody = append(secBody, keyNum[:]...)
	secBody = append(secBody, priv...)

	id := keyIDHex(keyNum)
	// "minisign public key" rather than a name of this repository's own: the
	// stock loader skips this line entirely, and the phrasing is what a human
	// expects to see at the top of a .pub file.
	pubFile = untrustedPrefix + "minisign public key " + id + "\n" +
		base64.StdEncoding.EncodeToString(pubBody) + "\n"
	secFile = untrustedPrefix + "ccdaddy release secret key " + id + "\n" +
		base64.StdEncoding.EncodeToString(secBody) + "\n"
	return pubFile, secFile, nil
}
