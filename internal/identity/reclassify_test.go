package identity

import "testing"

func TestReclassifyOnUsageNeedsPositiveEvidence(t *testing.T) {
	cases := []struct {
		name      string
		current   Kind
		usage     UsageShape
		want      Kind
		wantMoved bool
	}{
		{
			name:      "windows make it a subscription",
			current:   KindCredit,
			usage:     UsageShape{HasSubscriptionWindows: true},
			want:      KindSubscription,
			wantMoved: true,
		},
		{
			name:      "overage on with no windows makes it credit",
			current:   KindSubscription,
			usage:     UsageShape{ExtraUsageEnabled: true},
			want:      KindCredit,
			wantMoved: true,
		},
		{
			name:      "windows win over overage",
			current:   KindCredit,
			usage:     UsageShape{HasSubscriptionWindows: true, ExtraUsageEnabled: true},
			want:      KindSubscription,
			wantMoved: true,
		},
		{
			// The reading was successful and said nothing either way -- an
			// account out of credits reports overage off and no windows.
			// Classify's no-evidence default would call that a subscription and
			// walk it past the credit gate.
			name:      "no evidence leaves a credit account alone",
			current:   KindCredit,
			usage:     UsageShape{},
			want:      KindCredit,
			wantMoved: false,
		},
		{
			name:      "no evidence leaves a subscription alone",
			current:   KindSubscription,
			usage:     UsageShape{},
			want:      KindSubscription,
			wantMoved: false,
		},
		{
			// No quota concept, so usage says nothing about it at all.
			name:      "an api-key account is never reclassified",
			current:   KindAPIKey,
			usage:     UsageShape{HasSubscriptionWindows: true},
			want:      KindAPIKey,
			wantMoved: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, moved := ReclassifyOnUsage(tc.current, tc.usage)
			if got != tc.want || moved != tc.wantMoved {
				t.Errorf("ReclassifyOnUsage() = %v, %v; want %v, %v", got, moved, tc.want, tc.wantMoved)
			}
		})
	}
}

// Reclassification is defined in terms of Classify so the two cannot drift into
// disagreeing about the same evidence.
func TestReclassifyAgreesWithClassifyWhereverThereIsEvidence(t *testing.T) {
	for _, u := range []UsageShape{
		{HasSubscriptionWindows: true},
		{ExtraUsageEnabled: true},
		{HasSubscriptionWindows: true, ExtraUsageEnabled: true},
	} {
		got, moved := ReclassifyOnUsage(KindSubscription, u)
		if !moved {
			t.Fatalf("%+v: moved = false with evidence present", u)
		}
		if want := Classify(nil, u, false); got != want {
			t.Errorf("%+v: ReclassifyOnUsage = %v, Classify = %v", u, got, want)
		}
	}
}
