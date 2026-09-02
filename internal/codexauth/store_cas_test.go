package codexauth_test

import (
	"path/filepath"
	"testing"

	"github.com/Kweiza/ccdaddy/internal/codexauth"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
)

// The store spells the Codex blob key and the refresh-token hash privately,
// because a later change makes this package import the store and the store can
// never import back. This test is the gate that keeps the two spellings
// together, and it works in both import directions because an external test
// package is outside the cycle.
//
// It asserts BEHAVIOUR rather than string equality: a mark written with the
// hash this package computes must be the mark the store accepts, and the same
// hash must be the one the account then reports needing a relogin for. A
// renamed key or a changed digest breaks it either way round.
func TestTheStoreAndThisPackageAgreeOnTheKeyAndTheHash(t *testing.T) {
	t.Setenv("CCDAD_HOME", filepath.Join(t.TempDir(), "ccdad"))
	s, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	cred := codexauth.Credential{
		AccessToken:  "AT-1",
		RefreshToken: "RT-1",
		AccountID:    "acct-1",
		UserID:       "u-1",
	}
	if err := s.Add(store.Account{UUID: "u-1", Provider: provider.Codex}, cred.ToBlob()); err != nil {
		t.Fatal(err)
	}

	mark := codexauth.RefreshTokenHash(cred.RefreshToken)
	if err := s.SetCodexReloginFor("u-1", mark, mark); err != nil {
		t.Fatalf("SetCodexReloginFor() = %v, want nil", err)
	}
	got, ok := s.Get("u-1")
	if !ok {
		t.Fatal("the account is gone")
	}
	if got.CodexReloginFor != mark {
		t.Fatalf("CodexReloginFor = %q, want %q -- the store reads the refresh token from a "+
			"different key, or hashes it differently, than this package writes it",
			got.CodexReloginFor, mark)
	}
	if !got.NeedsRelogin(mark) {
		t.Error("the account does not report needing a relogin for the mark just written for it")
	}
	if got.NeedsRelogin(codexauth.RefreshTokenHash("RT-2")) {
		t.Error("the account reports needing a relogin for a token it does not hold")
	}
}
