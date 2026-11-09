package switcher

import (
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/pollpolicy"
	"github.com/Kweiza/ccdaddy/internal/usage"
)

// A switch brings the new live account's next poll forward, so the work's first
// reading on it does not wait out the idle cadence it was read on before. The
// instant is pollpolicy.PromptPollAt's, so the serve floor and a 429's floor both
// hold; the switch only writes it.
func TestASwitchSchedulesAPromptPoll(t *testing.T) {
	isolate(t)
	seed(t, "u-1", "one@example.com")
	two := seed(t, "u-2", "two@example.com")
	liveAs(t, "u-1")
	fetched := time.Now().Add(-10 * time.Minute)
	idle := time.Now().Add(9 * time.Minute)
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		c.Put("u-2", usage.Entry{FetchedAt: fetched, NextPollAt: idle})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	before := time.Now()
	res, err := Execute(openStore(t), Request{Target: two, LiveUUID: "u-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != Switched || res.PromptPollErr != nil {
		t.Fatalf("outcome = %v, prompt poll error %v", res.Outcome, res.PromptPollErr)
	}
	var got usage.Entry
	if err := usage.WithCache(time.Second, func(c *usage.Cache) error {
		got, _ = c.Get("u-2")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := pollpolicy.PromptPollAt(fetched, idle, time.Time{}, before)
	if got.NextPollAt.Before(want) || got.NextPollAt.After(time.Now()) {
		t.Errorf("NextPollAt = %s from now, want the prompt poll (%s), not the idle %s",
			time.Until(got.NextPollAt), time.Until(want), time.Until(idle))
	}
}
