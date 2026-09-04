package codexproxy

import (
	"testing"
	"time"
)

// MarkLimitedFor refuses the empty uuid before it touches the map, and no other
// test in this package notices when that refusal goes away: delete the
// `uuid == ""` clause from the guard and every test in limitbook_test.go stays
// green, because not one of them ever marks an account with no uuid.
//
// The refusal has to hold here rather than at the callers because the book is a
// plain map keyed by uuid, and the empty string is a usable key. An entry under
// it would answer LimitedUntil("") with "limited" -- and the callers that read
// this book walk store rows and ask under whatever uuid the row carries, so one
// row that never got a uuid back from the endpoint would both write that entry
// and then be held out of rotation by it, for a window nothing could shorten
// and no operator could name. store.ValidateUUID refuses the empty string on
// the way into the store, which is exactly why this branch is easy to delete
// without anything else going red, and exactly why it needs a test of its own.
//
// It marks through BOTH doors deliberately. MarkLimited reaches the guard only
// by delegating to MarkLimitedFor today; if it is ever given a body of its own,
// this test is what says the guard has to come with it.
func TestAnEmptyUUIDIsNeverRecorded(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	var b LimitBook
	b.MarkLimited("", now.Add(time.Hour))
	b.MarkLimitedFor("", now.Add(time.Hour), false)
	if _, ok := b.LimitedUntil("", now); ok {
		t.Error("the empty uuid was recorded as a rate-limited account")
	}
}
