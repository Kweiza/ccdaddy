// Package mcpsrv is ccdad's Model Context Protocol server.
//
// It owns no logic of its own. Every tool handler builds a fresh ccdad command
// tree, runs an ordinary verb through it, and reports what that verb printed
// and exited with -- so the scoped-session refusal, the auto-start hook, the
// exit-code contract and the --json payloads all keep exactly one authority,
// and none of them has a second implementation here. The package never imports
// internal/cli; the executor arrives as a function value.
//
// WHAT THE TRANSPORT DEPENDENCY COSTS, measured rather than assumed.
//
// The Model Context Protocol Go SDK replaces the standard library's
// encoding/json with segmentio/encoding, through an unexported internal
// package, with no build tag to opt out. That is 34 files importing unsafe and
// 310 symbols linked into a binary that holds live OAuth refresh tokens, on the
// code path that parses untrusted standard input. There is no supported way to
// take the SDK and decline it.
//
// The rest of the picture, because half of it is the reason this was accepted:
// golang.org/x/oauth2 is in the module graph and links ZERO symbols after
// dead-code elimination; os/exec links zero from the SDK's side; and no
// net/http server is linked at all -- the stdio transport is a pipe, not a
// listener, so nothing in this binary opens a port because of it. The exposure
// is one library on one path, not a web server bolted to a credential manager.
//
// It was taken for validation, not for protocol. A hand-rolled 102-line
// standard-library server spoke correct protocol to a real client and returned
// a SILENTLY WRONG ANSWER -- an empty account name -- for an argument of 42
// where a string was declared, while the SDK returned a typed tool error with
// the handler never invoked. For a surface that rewrites live credentials,
// silently coercing a malformed argument is the failure class that cannot ship.
//
// If that exposure is ever judged unacceptable the decision flips back to the
// hand-roll, and it has to flip before code is written against the SDK's
// AddTool: afterwards the flip costs the tool layer, the schema derivation and
// the test harness together.
package mcpsrv
