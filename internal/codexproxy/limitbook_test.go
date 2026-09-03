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
