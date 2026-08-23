package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/ccpath"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// bootstrapDocument is one account's worth of `ccdad export --full`, written to
// a file. writeImportFile is import_test.go's, and reusing it is deliberate: a
// document bootstrap accepts and import does not would be a second dialect of
// the same format.
func bootstrapDocument(t *testing.T, uuid, email string) string {
	t.Helper()
	return writeImportFile(t, `{
	  "schemaVersion": 1, "full": true,
	  "accounts": [{"uuid":"`+uuid+`","email":"`+email+`","kind":"subscription",
	    "credentials":{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-`+uuid+`"}}}]
	}`)
}

func accountCount(t *testing.T) int {
	t.Helper()
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	return len(s.Accounts())
}

// The four states CCDAD_IMPORT can be in when a container entrypoint runs this
// command unconditionally. The first two are the reason it is a command rather
// than a shell test in the entrypoint: an image built for a deployment with no
// document has to start, and `docker run -e CCDAD_IMPORT` with no value sets it
// to the empty string rather than leaving it out.
func TestBootstrapReadsCCDADImportOrDoesNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		set      func(t *testing.T)
		want     ExitCode
		accounts int
	}{{
		name:     "unset",
		set:      func(t *testing.T) { unsetForTest(t, "CCDAD_IMPORT") },
		want:     ExitOK,
		accounts: 0,
	}, {
		name:     "set but empty",
		set:      func(t *testing.T) { t.Setenv("CCDAD_IMPORT", "") },
		want:     ExitOK,
		accounts: 0,
	}, {
		name:     "set to a document",
		set:      func(t *testing.T) { t.Setenv("CCDAD_IMPORT", bootstrapDocument(t, "u-1", "one@example.com")) },
		want:     ExitOK,
		accounts: 1,
	}, {
		// 1 rather than 2: the document named does not exist, which is a
		// runtime failure the way `ccdad import` reports it, not a malformed
		// document. An entrypoint under `set -e` stops either way, which is
		// right — a mount that did not happen is not something to boot past.
		name:     "set to a path that is not there",
		set:      func(t *testing.T) { t.Setenv("CCDAD_IMPORT", filepath.Join(t.TempDir(), "nope.json")) },
		want:     ExitFailure,
		accounts: 0,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			tc.set(t)

			code, stdout, stderr, top := runRoot(t, "bootstrap")

			if code != tc.want {
				t.Fatalf("exit = %d, want %d\nstderr: %s\ntop: %s", code, tc.want, stderr, top)
			}
			if got := accountCount(t); got != tc.accounts {
				t.Fatalf("the store holds %d account(s), want %d", got, tc.accounts)
			}
			// Nothing on stdout, ever. An entrypoint runs this before the
			// command the container was started for, and a line here lands in
			// that command's stream.
			if stdout != "" {
				t.Fatalf("stdout = %q, want nothing: bootstrap runs ahead of the container's own command", stdout)
			}
		})
	}
}

// The silent half of "an entrypoint can call it unconditionally": with nothing
// to import there is nothing to say either, so a container's first log lines
// are its own.
func TestBootstrapSaysNothingWithNoDocument(t *testing.T) {
	isolate(t)
	unsetForTest(t, "CCDAD_IMPORT")

	code, stdout, stderr, top := runRoot(t, "bootstrap")

	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (%s)", code, ExitOK, top)
	}
	if stdout != "" || stderr != "" || top != "" {
		t.Fatalf("bootstrap spoke with no document set:\nstdout %q\nstderr %q\ntop %q", stdout, stderr, top)
	}
}

// A path, or "-" for stdin. The stdin form is what lets a document reach a
// container without ever being written to its filesystem.
func TestBootstrapReadsTheDocumentFromStdin(t *testing.T) {
	isolate(t)
	t.Setenv("CCDAD_IMPORT", "-")

	root := NewRootCmd()
	root.SetIn(strings.NewReader(`{
	  "schemaVersion": 1, "full": true,
	  "accounts": [{"uuid":"u-1","email":"one@example.com","kind":"subscription",
	    "credentials":{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-u-1"}}}]
	}`))
	root.SetArgs(explicitArgs([]string{"bootstrap"}))
	var out, errOut, top bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)

	if code := ExecuteWith(root, &top); code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s\ntop: %s", code, ExitOK, errOut.String(), top.String())
	}
	if got := accountCount(t); got != 1 {
		t.Fatalf("the store holds %d account(s), want the one piped in", got)
	}
}

// The document holds live refresh tokens, and this command's output is a
// container log — shipped, aggregated and kept. `ccdad import` names the
// accounts it applied because a person typed the path; here the same sentence
// would put an email address and an alias into a log that outlives the
// container, next to a message about the file the tokens are in.
//
// The refusal is the half that is easy to get wrong: validateExport's message
// quotes the uuid or the alias it refused, so it cannot be passed through.
func TestBootstrapPutsNothingFromTheDocumentInItsOutput(t *testing.T) {
	const (
		email   = "leak@example.com"
		alias   = "leakyalias"
		refresh = "RT-DO-NOT-LOG-THIS"
	)
	for _, tc := range []struct {
		name string
		body string
		want ExitCode
	}{{
		name: "applied",
		body: `{"schemaVersion":1,"full":true,"accounts":[
		  {"uuid":"u-1","email":"` + email + `","alias":"` + alias + `","kind":"subscription",
		   "credentials":{"claudeAiOauth":{"accessToken":"AT","refreshToken":"` + refresh + `"}}}]}`,
		want: ExitOK,
	}, {
		// store.ValidateAlias refuses this one and QUOTES IT BACK, which is the
		// whole hazard: the alias is a value out of the document, sitting in a
		// message about the file the refresh tokens are in. A refusal that
		// quoted something the document did not supply -- "7 is all digits" --
		// would leave this assertion satisfied by a command that leaks.
		name: "refused",
		body: `{"schemaVersion":1,"full":true,"accounts":[
		  {"uuid":"u-1","email":"` + email + `","alias":"` + alias + `!","kind":"subscription",
		   "credentials":{"claudeAiOauth":{"accessToken":"AT","refreshToken":"` + refresh + `"}}}]}`,
		want: ExitUsage,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			t.Setenv("CCDAD_IMPORT", writeImportFile(t, tc.body))

			code, stdout, stderr, top := runRoot(t, "bootstrap")

			if code != tc.want {
				t.Fatalf("exit = %d, want %d\nstderr: %s\ntop: %s", code, tc.want, stderr, top)
			}
			said := stdout + stderr + top
			for _, secret := range []string{email, alias, refresh} {
				if strings.Contains(said, secret) {
					t.Errorf("bootstrap put %q from the document into its output:\n%s", secret, said)
				}
			}
			// And it has to say SOMETHING, or the rule above is satisfied by a
			// command that has gone silent on a document it refused.
			if tc.want != ExitOK && !strings.Contains(said, "CCDAD_IMPORT") {
				t.Errorf("the refusal never names the variable that carried the document:\n%s", said)
			}
		})
	}
}

// Running it twice over the same document leaves one account and does not move
// its age. Both halves matter: an entrypoint runs this on every container start,
// so a second run that duplicated an account would grow the store without bound,
// and one that re-stamped AddedAt would make every account look new forever.
func TestBootstrapIsIdempotent(t *testing.T) {
	isolate(t)
	t.Setenv("CCDAD_IMPORT", bootstrapDocument(t, "u-1", "one@example.com"))

	if code, _, _, top := runRoot(t, "bootstrap"); code != ExitOK {
		t.Fatalf("first run = %d (%s), want 0", code, top)
	}
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	first, ok := s.Get("u-1")
	if !ok {
		t.Fatal("the first run stored nothing")
	}

	if code, _, _, top := runRoot(t, "bootstrap"); code != ExitOK {
		t.Fatalf("second run = %d (%s), want 0", code, top)
	}

	reopened, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reopened.Accounts()); got != 1 {
		t.Fatalf("the store holds %d account(s) after two runs of one document, want 1", got)
	}
	second, _ := reopened.Get("u-1")
	if !second.AddedAt.Equal(first.AddedAt) {
		t.Fatalf("AddedAt = %v after a second bootstrap, want the original %v", second.AddedAt, first.AddedAt)
	}
}

// The restart case, and the reason this command does not answer 3. The daemon
// refreshes an account's token while the container runs; the mounted document
// still holds the older one; on the next start every account is left alone. That
// is the store being exactly as asked for, and an entrypoint under `set -e`
// would treat a 3 here as a reason not to start the container at all.
func TestBootstrapExitsZeroWhenEveryAccountIsLeftAlone(t *testing.T) {
	isolate(t)
	local := time.Now().Add(6 * time.Hour).UnixMilli()
	older := time.Now().Add(-2 * time.Hour).UnixMilli()

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(store.Account{UUID: "u-1", Email: "one@example.com"},
		credsWithExpiry("RT-local", local)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCDAD_IMPORT", writeImportFile(t, fmt.Sprintf(`{
	  "schemaVersion": 1, "full": true,
	  "accounts": [{"uuid":"u-1","email":"one@example.com","kind":"subscription",
	    "credentials":{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-stale","expiresAt":%d}}}]
	}`, older)))

	code, _, stderr, top := runRoot(t, "bootstrap")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d: an entrypoint under `set -e` stops the container on anything else\n%s%s",
			code, ExitOK, stderr, top)
	}
	reopened, _ := store.Open()
	got, _ := reopened.Credentials("u-1")
	if !strings.Contains(string(got["claudeAiOauth"]), "RT-local") {
		t.Fatalf("the newer local credential was overwritten by the mounted document: %s", got["claudeAiOauth"])
	}
}

// The other refusal, and the one the IsUsageError arm exists for. An alias the
// document hands to one account while an account already here holds it is
// refused inside the store transaction rather than by validateExport, and that
// message names the alias out of the document AND the local account holding it.
// Both are exactly what must not reach a container log.
func TestBootstrapNamesNothingWhenAnAliasCollides(t *testing.T) {
	const (
		alias    = "sharedhandle"
		incoming = "leak@example.com"
		local    = "held@example.com"
	)
	isolate(t)
	seedAccount(t, "u-held", local)
	if code, _, _, top := runRoot(t, "alias", "1", alias); code != ExitOK {
		t.Fatalf("alias = %d (%s)", code, top)
	}
	t.Setenv("CCDAD_IMPORT", writeImportFile(t, `{"schemaVersion":1,"full":true,"accounts":[
	  {"uuid":"u-1","email":"`+incoming+`","alias":"`+alias+`","kind":"subscription",
	   "credentials":{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-u-1"}}}]}`))

	code, stdout, stderr, top := runRoot(t, "bootstrap")

	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d\nstderr: %s\ntop: %s", code, ExitUsage, stderr, top)
	}
	said := stdout + stderr + top
	for _, secret := range []string{alias, incoming, local, "u-held"} {
		if strings.Contains(said, secret) {
			t.Errorf("bootstrap put %q into its output:\n%s", secret, said)
		}
	}
	if !strings.Contains(said, "CCDAD_IMPORT") {
		t.Errorf("the refusal never names the variable that carried the document:\n%s", said)
	}
	if got := accountCount(t); got != 1 {
		t.Fatalf("the store holds %d account(s), want only the one that was already here", got)
	}
}

// A document carrying MCP logins gets one fixed sentence about them. It is
// worth saying because it describes something that did NOT happen and that a
// reader restoring a backup will otherwise go looking for, and it is allowed
// past the no-content rule precisely because it carries nothing out of the
// document.
func TestBootstrapSaysMCPLoginsAreNotInstalled(t *testing.T) {
	isolate(t)
	t.Setenv("CCDAD_IMPORT", writeImportFile(t, `{"schemaVersion":1,"full":true,
	  "machine":{"mcpOAuth":{"srv":{"accessToken":"MCP-DO-NOT-LOG-THIS"}}},
	  "accounts":[{"uuid":"u-1","email":"one@example.com","kind":"subscription",
	    "credentials":{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-u-1"}}}]}`))

	code, stdout, stderr, top := runRoot(t, "bootstrap")

	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s\ntop: %s", code, ExitOK, stderr, top)
	}
	if !strings.Contains(stderr, "MCP logins") {
		t.Errorf("stderr = %q, want it to say the MCP logins are not being installed", stderr)
	}
	if strings.Contains(stdout+stderr+top, "MCP-DO-NOT-LOG-THIS") {
		t.Errorf("the note carried a value out of the document:\n%s", stdout+stderr+top)
	}
	// And the sentence is true: nothing was written into Claude Code's own
	// credentials file, which is where an installed MCP login would land.
	assertNoLiveCredentials(t)

	// The note is not printed for a document with no machine half, or it stops
	// carrying information.
	isolate(t)
	t.Setenv("CCDAD_IMPORT", bootstrapDocument(t, "u-1", "one@example.com"))
	if _, _, stderr, _ := runRoot(t, "bootstrap"); strings.Contains(stderr, "MCP logins") {
		t.Errorf("stderr = %q, want no MCP note for a document that carries none", stderr)
	}
}

// The alias collision checkAliasCollisions does NOT catch, and the one a
// container walks into on an ordinary restart.
//
// That check excludes every local account the document also names, on the
// assumption that the first apply pass clears its alias. A row SKIPPED because
// the credentials here are newer never reaches that pass, so its alias is still
// held when a later row asks for it — and store.setAlias refuses mid-batch with
// a message naming the alias out of the document and the local account holding
// it. Both of those are the values this command exists not to log, and the
// trigger is the same "the daemon refreshed a token while the container ran"
// state the never-answer-3 rule was written for.
func TestBootstrapNamesNothingWhenAnAliasCollidesMidBatch(t *testing.T) {
	const (
		alias    = "sharedhandle"
		local    = "alpha@example.com"
		incoming = "beta@example.com"
		refresh  = "RT-DO-NOT-LOG-THIS"
	)
	isolate(t)
	newer := time.Now().Add(6 * time.Hour).UnixMilli()
	older := time.Now().Add(-2 * time.Hour).UnixMilli()

	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(store.Account{UUID: "u-alpha", Email: local}, credsWithExpiry("RT-local", newer)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAlias("u-alpha", alias); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CCDAD_IMPORT", writeImportFile(t, fmt.Sprintf(`{
	  "schemaVersion":1,"full":true,"accounts":[
	    {"uuid":"u-alpha","email":%q,"kind":"subscription",
	     "credentials":{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-stale","expiresAt":%d}}},
	    {"uuid":"u-beta","email":%q,"alias":%q,"kind":"subscription",
	     "credentials":{"claudeAiOauth":{"accessToken":"AT","refreshToken":%q,"expiresAt":%d}}}]}`,
		local, older, incoming, alias, refresh, older)))

	code, stdout, stderr, top := runRoot(t, "bootstrap")

	said := stdout + stderr + top
	for _, secret := range []string{alias, local, incoming, refresh, "u-alpha", "u-beta"} {
		if strings.Contains(said, secret) {
			t.Errorf("bootstrap put %q into its output:\n%s", secret, said)
		}
	}
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d: the document is what is wrong, not this machine\n%s", code, ExitUsage, said)
	}
	if got := accountCount(t); got != 1 {
		t.Errorf("the store holds %d account(s), want only the one that was already here", got)
	}
	// And nothing was half-written. store.Add writes the credential file before
	// it touches memory, so a batch refused after the add leaves a live refresh
	// token in a file no accounts.toml names.
	orphan := filepath.Join(mustPath(ccpath.StoreHome()), "credentials", "u-beta.json")
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("a credential file holding a live refresh token was left at %s with no account naming it", orphan)
	}
}

// readExport's own refusals reach this command too, and json.Unmarshal's error
// describes the bytes it choked on. A file that is nothing but a refresh token
// produces `invalid character 'R' looking for beginning of value` — the parser
// reading the document back out loud, into a log.
func TestBootstrapDoesNotRepeatWhyTheDocumentWouldNotParse(t *testing.T) {
	isolate(t)
	t.Setenv("CCDAD_IMPORT", writeImportFile(t, "RT-DO-NOT-LOG-THIS"))

	code, stdout, stderr, top := runRoot(t, "bootstrap")

	said := stdout + stderr + top
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d: the document is what is wrong\n%s", code, ExitUsage, said)
	}
	if strings.Contains(said, "invalid character") {
		t.Errorf("bootstrap repeated the parser's reading of the document:\n%s", said)
	}
	if !strings.Contains(said, "CCDAD_IMPORT") {
		t.Errorf("the refusal never names the variable that carried the document:\n%s", said)
	}
}

// A document from a newer ccdad is accepted, and saying so is worth a line —
// an operator whose image is older than their backup has no other way to learn
// it. The version number is not: it is a value out of the document, and the
// fact travels without it.
func TestBootstrapNotesANewerSchemaWithoutQuotingIt(t *testing.T) {
	isolate(t)
	t.Setenv("CCDAD_IMPORT", writeImportFile(t, `{"schemaVersion":99,"full":true,"accounts":[
	  {"uuid":"u-1","email":"one@example.com","kind":"subscription",
	   "credentials":{"claudeAiOauth":{"accessToken":"AT","refreshToken":"RT-u-1"}}}]}`))

	code, stdout, stderr, top := runRoot(t, "bootstrap")

	if code != ExitOK {
		t.Fatalf("exit = %d, want %d\nstderr: %s\ntop: %s", code, ExitOK, stderr, top)
	}
	if !strings.Contains(stderr, "newer ccdad") {
		t.Errorf("stderr = %q, want it to say the document is from a newer build", stderr)
	}
	if strings.Contains(stdout+stderr+top, "99") {
		t.Errorf("bootstrap quoted the document's schema version:\n%s", stdout+stderr+top)
	}
}
