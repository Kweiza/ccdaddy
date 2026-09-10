package view

import (
	"strings"
	"testing"
	"time"

	"github.com/Kweiza/ccdaddy/internal/daemon"
	"github.com/Kweiza/ccdaddy/internal/provider"
	"github.com/Kweiza/ccdaddy/internal/store"
)

func TestCanceledSubscriptionsOverrideAStaleEngineStateOnThePage(t *testing.T) {
	r := Row{Account: store.Account{Provider: provider.Claude, SubscriptionStatus: "canceled"}, Engine: daemon.AccountStatus{State: daemon.StateCandidate}}
	if got := r.ListCell(ListColumn{Kind: ColumnState}, Columns{}, time.Now(), false); got != "unsubscribed" {
		t.Fatalf("state = %q", got)
	}
	if r.AutoLabel() != "no" || !strings.Contains(r.StatusFlags(), "subscription canceled") {
		t.Fatalf("expired subscription is still displayed as eligible: %+v", r)
	}
	r.Account.SubscriptionStatus = "active"
	if r.AutoLabel() != "yes" {
		t.Fatal("renewal did not restore eligibility")
	}
}
