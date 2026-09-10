package identity

import (
	"context"
	"io"
	"net/http"
	"testing"
)

func TestProfileReadsTheCanceledSubscriptionEvenWithAnOldPaidTier(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"account":{"uuid":"acct-1"},"organization":{"organization_type":"claude_max","billing_type":"none","subscription_status":"canceled"}}`)
	})
	p, err := c.FetchProfile(context.Background(), "TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if p.SubscriptionStatus != "canceled" {
		t.Fatalf("subscription status = %q", p.SubscriptionStatus)
	}
}
