package store

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Kweiza/ccdaddy/internal/provider"
)

// minUUIDPrefix is the shortest uuid prefix Resolve will accept. A switch
// overwrites the live credentials file, so a near-miss must not resolve.
const minUUIDPrefix = 8

var (
	// ErrNotFound means no account matched the reference.
	ErrNotFound = errors.New("no such account")
	// ErrAmbiguous means more than one account matched.
	ErrAmbiguous = errors.New("that reference matches more than one account")
	// ErrBadAlias means an alias failed validation.
	ErrBadAlias = errors.New("invalid alias")
	// ErrAliasTaken means the alias is well-formed but another account holds
	// it. It is separate from ErrBadAlias so a caller does not answer a
	// collision with advice about the charset.
	ErrAliasTaken = errors.New("alias already in use")
)

// NormalizeAlias lowercases and trims an alias to its stored form.
func NormalizeAlias(alias string) string {
	return strings.ToLower(strings.TrimSpace(alias))
}

// ValidateAlias enforces the alias rules against the NORMALIZED form, so a
// caller that validates a flag value before touching the store gets the same
// verdict SetAlias would give it.
//
// Three of the rules are structural rather than cosmetic. An alias may not be
// purely numeric, because Resolve tries an index first and the two axes would
// otherwise name different accounts for the same token. An alias may not spell
// a provider-scoped ordinal either -- "c1", "x12" -- for the same reason and
// against the same axis: those are what a per-provider index is typed as. An
// alias may not start with a hyphen, so `ccdad run <ACCT>` can reject a
// leading-hyphen token as a usage error rather than mistaking a flag for an
// account.
//
// The ordinal rule is enforced HERE and not in Resolve, which still tries the
// alias first. An alias of that shape stored by an older build goes on naming
// the account its owner gave it -- taking a working handle away on upgrade is a
// worse answer than letting one legacy alias shadow a reference that has a
// second spelling -- while no new one can be created to shadow anything.
func ValidateAlias(alias string) error {
	a := NormalizeAlias(alias)
	if a == "" {
		return fmt.Errorf("%w: an alias cannot be empty", ErrBadAlias)
	}
	if strings.HasPrefix(a, "-") {
		return fmt.Errorf("%w: %q cannot start with '-', or it would be read as a flag", ErrBadAlias, a)
	}
	if isAllDigits(a) {
		return fmt.Errorf("%w: %q is all digits, which is reserved for the display index", ErrBadAlias, a)
	}
	if _, _, ok := parseOrdinal(a); ok {
		return fmt.Errorf("%w: %q is a provider and a display index, which is reserved for referring to that account", ErrBadAlias, a)
	}
	for _, r := range a {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if !ok {
			return fmt.Errorf("%w: %q may only contain a-z, 0-9, '.', '_' and '-'", ErrBadAlias, a)
		}
	}
	return nil
}

// isAllDigits is the literal test for step 1 of the resolution order.
// strconv.Atoi is not a substitute: it accepts "+2" and rejects a 20-digit run
// with ErrRange.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseOrdinal reads a provider-scoped display reference -- "c1", "x12" -- into
// the provider it names and the number under it.
//
// It is deliberately strict about the shape: one known prefix letter, then at
// least one digit and nothing but digits. strconv.Atoi is not enough on its own
// for the reason isAllDigits exists -- it accepts "c+2" once the letter is cut
// -- and a loose reading here would turn a uuid prefix beginning with 'c' into
// an ordinal for an account nobody named.
//
// The number is NOT range-checked against a fleet. That is Resolve's answer to
// give, and it gives it by finding no account.
func parseOrdinal(s string) (provider.ID, int, bool) {
	var p provider.ID
	switch {
	case strings.HasPrefix(s, ClaudePrefix):
		p = provider.Claude
	case strings.HasPrefix(s, CodexPrefix):
		p = provider.Codex
	default:
		return "", 0, false
	}
	digits := s[len(ProviderPrefix(p)):]
	if !isAllDigits(digits) {
		return "", 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return "", 0, false
	}
	return p, n, true
}

// Resolve maps a user-typed reference to exactly one account.
//
// Order: index, then alias, then provider-scoped index, then email, then uuid
// prefix. There is no substring or fuzzy matching at any step.
//
// The bare index is FIRST and yet may match twice, which is the shape the
// per-provider ordinal forced. It is answered by naming both candidates and the
// two spellings that separate them, never by picking one: the commands that
// take a reference overwrite a credentials file, and a coin flip between two
// accounts is the one outcome none of them may have.
func Resolve(accounts []Account, ref string) (Account, error) {
	needle := strings.TrimSpace(ref)
	if needle == "" {
		return Account{}, fmt.Errorf("%w: no account given", ErrNotFound)
	}

	// 1. All digits: a display index. It is per-provider now, so a number can
	// name one account under each provider and the count decides the answer.
	//
	// A miss still falls THROUGH rather than erroring, because a uuid may
	// legitimately begin with eight digits and the uuid step must get a chance
	// at it. Falling through cannot mis-resolve: an alias may not be purely
	// numeric and an email always carries '@', so only a uuid prefix is
	// reachable from here.
	if isAllDigits(needle) {
		if n, err := strconv.Atoi(needle); err == nil {
			var byIdx []Account
			for _, a := range accounts {
				if a.Idx == n {
					byIdx = append(byIdx, a)
				}
			}
			if len(byIdx) == 1 {
				return byIdx[0], nil
			}
			if len(byIdx) > 1 {
				return Account{}, ambiguousOrdinal(needle, byIdx)
			}
		}
	}

	lower := strings.ToLower(needle)

	// 2. Exact alias. Ahead of the provider-scoped index so that an alias of
	// that shape, stored before ValidateAlias began refusing the shape, goes on
	// naming the account its owner gave it.
	for _, a := range accounts {
		if a.Alias != "" && strings.ToLower(a.Alias) == lower {
			return a, nil
		}
	}

	// 3. A provider and a display index together: the spelling that always
	// names one account, and the one the ambiguity above tells the user to
	// reach for. A miss falls through for step 1's reason -- a uuid may begin
	// with 'c' or 'x' followed by digits.
	if p, n, ok := parseOrdinal(lower); ok {
		for _, a := range accounts {
			if a.Provider == p && a.Idx == n {
				return a, nil
			}
		}
	}

	// 4. Exact email. A repeat across organizations is legitimate, so more than
	// one match is an error naming each candidate rather than a prompt.
	var byEmail []Account
	for _, a := range accounts {
		if a.Email != "" && strings.ToLower(a.Email) == lower {
			byEmail = append(byEmail, a)
		}
	}
	if len(byEmail) == 1 {
		return byEmail[0], nil
	}
	if len(byEmail) > 1 {
		parts := make([]string, 0, len(byEmail))
		for _, a := range byEmail {
			org := a.OrganizationUUID
			if org == "" {
				org = "unknown organization"
			}
			parts = append(parts, fmt.Sprintf("%s (%s)", a.Ref(), org))
		}
		return Account{}, fmt.Errorf("%w: %q is used by %s. Use the reference, or set an alias with 'ccdad alias'",
			ErrAmbiguous, needle, strings.Join(parts, ", "))
	}

	// 5. A uuid, or a unique prefix of at least minUUIDPrefix characters.
	if len(lower) >= minUUIDPrefix {
		var byUUID []Account
		for _, a := range accounts {
			if strings.HasPrefix(strings.ToLower(a.UUID), lower) {
				byUUID = append(byUUID, a)
			}
		}
		if len(byUUID) == 1 {
			return byUUID[0], nil
		}
		if len(byUUID) > 1 {
			parts := make([]string, 0, len(byUUID))
			for _, a := range byUUID {
				parts = append(parts, fmt.Sprintf("%s=%s (%s)", a.Ref(), a.Label(), a.UUID))
			}
			return Account{}, fmt.Errorf("%w: %q is a prefix of %s; use more characters",
				ErrAmbiguous, needle, strings.Join(parts, ", "))
		}
	}

	return Account{}, fmt.Errorf("%w: %q. %s", ErrNotFound, needle, available(accounts))
}

// ambiguousOrdinal is what a bare number says when both providers number an
// account with it.
//
// It names each candidate by the reference that WOULD have worked, so the fix
// is on the screen rather than in the help: a user who typed 2 reads back c2
// and x2 with a label against each and re-runs the command with one of them.
func ambiguousOrdinal(needle string, matches []Account) error {
	parts := make([]string, 0, len(matches))
	for _, a := range matches {
		parts = append(parts, fmt.Sprintf("%s=%s", a.Ref(), a.Label()))
	}
	return fmt.Errorf("%w: the display index is per provider, and %q numbers one account under each: %s. "+
		"Use one of those, or an alias or uuid", ErrAmbiguous, needle, strings.Join(parts, ", "))
}

func available(accounts []Account) string {
	// Resolution is provider-blind, and on an empty store there is nothing to
	// guess a provider from, so both logins are named rather than one.
	if len(accounts) == 0 {
		return "No accounts are managed yet — run 'ccdad add claude' or 'ccdad add codex'."
	}
	// By Ref and not by Idx. This list is not grouped by provider, so a bare
	// number in it would appear twice against two different accounts -- which
	// is the exact confusion the prefix exists to end, printed by the error
	// whose job is to end it.
	parts := make([]string, 0, len(accounts))
	for _, a := range accounts {
		parts = append(parts, fmt.Sprintf("%s=%s", a.Ref(), a.Label()))
	}
	return "Available: " + strings.Join(parts, ", ") + "."
}
