package store

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
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
// Two of the rules are structural rather than cosmetic. An alias may not be
// purely numeric, because Resolve tries an index first and the two axes would
// otherwise name different accounts for the same token. An alias may not start
// with a hyphen, so `ccdad run <ACCT>` can reject a leading-hyphen token as a
// usage error rather than mistaking a flag for an account.
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

// Resolve maps a user-typed reference to exactly one account.
//
// Order: index, then alias, then email, then uuid prefix. There is no substring
// or fuzzy matching at any step.
func Resolve(accounts []Account, ref string) (Account, error) {
	needle := strings.TrimSpace(ref)
	if needle == "" {
		return Account{}, fmt.Errorf("%w: no account given", ErrNotFound)
	}

	// 1. All digits: a display index. A miss falls THROUGH rather than
	// erroring, because a uuid may legitimately begin with eight digits and
	// step 4 must still get a chance at it. Falling through cannot mis-resolve:
	// an alias may not be purely numeric and an email always carries '@', so
	// only a uuid prefix is reachable from here.
	if isAllDigits(needle) {
		if n, err := strconv.Atoi(needle); err == nil {
			for _, a := range accounts {
				if a.Idx == n {
					return a, nil
				}
			}
		}
	}

	lower := strings.ToLower(needle)

	// 2. Exact alias.
	for _, a := range accounts {
		if a.Alias != "" && strings.ToLower(a.Alias) == lower {
			return a, nil
		}
	}

	// 3. Exact email. A repeat across organizations is legitimate, so more than
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
			parts = append(parts, fmt.Sprintf("%d (%s)", a.Idx, org))
		}
		return Account{}, fmt.Errorf("%w: %q is used by %s. Use the index, or set an alias with 'ccdad alias'",
			ErrAmbiguous, needle, strings.Join(parts, ", "))
	}

	// 4. A uuid, or a unique prefix of at least minUUIDPrefix characters.
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
				parts = append(parts, fmt.Sprintf("%d=%s (%s)", a.Idx, a.Label(), a.UUID))
			}
			return Account{}, fmt.Errorf("%w: %q is a prefix of %s; use more characters",
				ErrAmbiguous, needle, strings.Join(parts, ", "))
		}
	}

	return Account{}, fmt.Errorf("%w: %q. %s", ErrNotFound, needle, available(accounts))
}

func available(accounts []Account) string {
	// Resolution is provider-blind, and on an empty store there is nothing to
	// guess a provider from, so both logins are named rather than one.
	if len(accounts) == 0 {
		return "No accounts are managed yet — run 'ccdad add claude' or 'ccdad add codex'."
	}
	parts := make([]string, 0, len(accounts))
	for _, a := range accounts {
		parts = append(parts, fmt.Sprintf("%d=%s", a.Idx, a.Label()))
	}
	return "Available: " + strings.Join(parts, ", ") + "."
}
