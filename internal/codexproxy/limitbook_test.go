package codexproxy

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

var bookEpoch = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

func TestAnAccountNobodyMarkedIsNotLimited(t *testing.T) {
	var b LimitBook
	if until, ok := b.LimitedUntil("cx-1", bookEpoch); ok {
		t.Fatalf("LimitedUntil = (%v, true), want no limit", until)
	}
}

func TestAMarkedAccountIsLimitedUntilItsInstant(t *testing.T) {
	var b LimitBook
	until := bookEpoch.Add(30 * time.Minute)
	b.MarkLimited("cx-1", until)

	got, ok := b.LimitedUntil("cx-1", bookEpoch)
	if !ok || !got.Equal(until) {
		t.Fatalf("LimitedUntil = (%v, %v), want (%v, true)", got, ok, until)
	}
}

// The limit lapses on its own. Nothing sweeps this book, so a record that kept
// answering after its instant would hold an account out for the life of the
// daemon on the strength of one 429.
func TestALapsedLimitAnswersNo(t *testing.T) {
	var b LimitBook
	until := bookEpoch.Add(30 * time.Minute)
	b.MarkLimited("cx-1", until)

	if got, ok := b.LimitedUntil("cx-1", until); ok {
		t.Fatalf("LimitedUntil at the instant itself = (%v, true), want no limit", got)
	}
	if got, ok := b.LimitedUntil("cx-1", until.Add(time.Second)); ok {
		t.Fatalf("LimitedUntil after the instant = (%v, true), want no limit", got)
	}
}

// Two 429s for one account, the second offering a SHORTER wait. The longer one
// stands: the lane and the proxy both write here, and letting the second write
// shorten the wait would send the next request back into the throttle the first
// one measured.
func TestAShorterLimitDoesNotShortenTheWait(t *testing.T) {
	var b LimitBook
	long := bookEpoch.Add(time.Hour)
	b.MarkLimited("cx-1", long)
	b.MarkLimited("cx-1", bookEpoch.Add(time.Minute))

	got, ok := b.LimitedUntil("cx-1", bookEpoch)
	if !ok || !got.Equal(long) {
		t.Fatalf("LimitedUntil = (%v, %v), want (%v, true) -- the longer wait stands", got, ok, long)
	}
}

func TestALaterLimitExtendsTheWait(t *testing.T) {
	var b LimitBook
	b.MarkLimited("cx-1", bookEpoch.Add(time.Minute))
	longer := bookEpoch.Add(time.Hour)
	b.MarkLimited("cx-1", longer)

	got, ok := b.LimitedUntil("cx-1", bookEpoch)
	if !ok || !got.Equal(longer) {
		t.Fatalf("LimitedUntil = (%v, %v), want (%v, true)", got, ok, longer)
	}
}

// A nil book is "nothing recorded" rather than a panic. `ccdad switch` ranks
// codex accounts in a process that has no proxy in it at all, and it passes nil.
func TestANilBookAnswersEveryQuestionWithNothing(t *testing.T) {
	var b *LimitBook
	b.MarkLimited("cx-1", bookEpoch.Add(time.Hour))
	if until, ok := b.LimitedUntil("cx-1", bookEpoch); ok {
		t.Fatalf("LimitedUntil on a nil book = (%v, true), want no limit", until)
	}
}

// MarkLimited is a one-line delegation to MarkLimitedFor, so the case above only
// proves MarkLimitedFor tolerates nil by way of that delegation. Call it
// directly too: a future edit that stops MarkLimited from delegating must not
// silently drop this method's own nil guard.
func TestANilBookToleratesMarkLimitedForDirectly(t *testing.T) {
	var b *LimitBook
	b.MarkLimitedFor("cx-1", bookEpoch.Add(time.Hour), false)
	if until, ok := b.LimitedUntil("cx-1", bookEpoch); ok {
		t.Fatalf("LimitedUntil on a nil book = (%v, true), want no limit", until)
	}
}

// ResetsAt is the fourth method and the one codex-facing answers must use, so
// it needs its own nil case rather than riding on LimitedUntil's.
func TestANilBookToleratesResetsAt(t *testing.T) {
	var b *LimitBook
	if until, ok := b.ResetsAt("cx-1", bookEpoch); ok {
		t.Fatalf("ResetsAt on a nil book = (%v, true), want no limit", until)
	}
}

// The proxy is a fully concurrent handler and the lane writes from its own
// goroutine, so this is read and written from several at once. -race is the
// assertion; the loop is only what gives it something to look at.
func TestTheBookIsSafeForConcurrentUse(t *testing.T) {
	var b LimitBook
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			uuid := fmt.Sprintf("cx-%d", i%4)
			for j := 0; j < 64; j++ {
				b.MarkLimited(uuid, bookEpoch.Add(time.Duration(j)*time.Second))
				b.LimitedUntil(uuid, bookEpoch)
			}
		}(i)
	}
	wg.Wait()
}

// ResetsAt is the only deadline an answer to codex may quote, and the whole
// point of the known flag is that a deadline ccdad invented never leaves the
// process wearing the endpoint's authority. Without this the flag ships
// unprotected: deleting the !e.known test leaves every other case in this file
// green, because nothing else ever reads a marked account back through
// ResetsAt.
func TestAnInventedDeadlineIsNeverQuotedToCodex(t *testing.T) {
	b := &LimitBook{}
	until := bookEpoch.Add(time.Hour)
	b.MarkLimitedFor("cx-1", until, false)

	if _, ok := b.LimitedUntil("cx-1", bookEpoch); !ok {
		t.Fatal("LimitedUntil said no; an invented deadline still holds the account back")
	}
	if at, ok := b.ResetsAt("cx-1", bookEpoch); ok {
		t.Errorf("ResetsAt quoted %v for a deadline the endpoint never stated", at)
	}
}

// And the other half: a reset the endpoint did state is quoted, right up to
// the instant it lapses and not past it.
func TestAnEndpointStatedResetIsQuoted(t *testing.T) {
	b := &LimitBook{}
	until := bookEpoch.Add(time.Hour)
	b.MarkLimitedFor("cx-1", until, true)

	if at, ok := b.ResetsAt("cx-1", bookEpoch); !ok || !at.Equal(until) {
		t.Errorf("ResetsAt = %v, %v; want %v, true", at, ok, until)
	}
	if _, ok := b.ResetsAt("cx-1", until); ok {
		t.Error("ResetsAt still quoted a reset at the very instant it lapsed")
	}
	if _, ok := b.ResetsAt("cx-1", until.Add(time.Nanosecond)); ok {
		t.Error("ResetsAt quoted a reset a nanosecond after it lapsed")
	}
}
