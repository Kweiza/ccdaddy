package identity

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		name     string
		profile  *Profile
		usage    UsageShape
		isAPIKey bool
		want     Kind
	}{
		{
			name:    "five hour and seven day windows mean a subscription",
			profile: &Profile{OrganizationType: "claude_max"},
			usage:   UsageShape{HasSubscriptionWindows: true},
			want:    KindSubscription,
		},
		{
			// Overage on top of a subscription is still a subscription: credits
			// are a secondary axis and are not touched while a window has room.
			name: "subscription with overage enabled is still a subscription",
			profile: &Profile{
				OrganizationType: "claude_enterprise",
				SeatTier:         "enterprise_usage_based",
				RateLimitTier:    "default_claude_zero",
				HasExtraUsage:    true,
			},
			usage: UsageShape{HasSubscriptionWindows: true, ExtraUsageEnabled: true},
			want:  KindSubscription,
		},
		{
			// An account that is both is a subscription. The login and add-token
			// paths classify with an empty UsageShape because they never fetch
			// usage, so this is the shape every `ccdad add` sees: the
			// organization's overage switch is the ONLY credit-looking signal
			// present, and it must not decide the verdict.
			name:    "a max org with overage enabled and no usage read is still a subscription",
			profile: &Profile{OrganizationType: "claude_max", BillingType: "subscription", HasExtraUsage: true},
			usage:   UsageShape{},
			want:    KindSubscription,
		},
		{
			// HasExtraUsage is deliberately left false: this case exists to pin
			// the usage-derived rule, and must fail if that branch is removed.
			name:    "no windows but extra usage enabled is a credit account",
			profile: &Profile{OrganizationType: "claude_enterprise"},
			usage:   UsageShape{HasSubscriptionWindows: false, ExtraUsageEnabled: true},
			want:    KindCredit,
		},
		{
			name:    "no windows and a metered billing type is a credit account",
			profile: &Profile{BillingType: "usage_based"},
			usage:   UsageShape{HasSubscriptionWindows: false},
			want:    KindCredit,
		},
		{
			// billing_type is upstream text, so the comparison normalizes.
			name:    "a metered billing type is matched after normalization",
			profile: &Profile{BillingType: "  Usage_Based  "},
			usage:   UsageShape{},
			want:    KindCredit,
		},
		{
			// An unrecognized billing_type falls through to the safe side
			// rather than being read as evidence of metering.
			name:    "an unrecognized billing type is not credit evidence",
			profile: &Profile{BillingType: "something_new"},
			usage:   UsageShape{},
			want:    KindSubscription,
		},
		{
			name:     "an api key has no quota concept",
			profile:  nil,
			usage:    UsageShape{},
			isAPIKey: true,
			want:     KindAPIKey,
		},
		{
			// isAPIKey outranks every other signal, including live windows.
			name:     "an api key stays an api key even with subscription windows",
			profile:  &Profile{OrganizationType: "claude_max"},
			usage:    UsageShape{HasSubscriptionWindows: true},
			isAPIKey: true,
			want:     KindAPIKey,
		},
		{
			// Unknown is not credit: treating an unreadable account as credit
			// would let it through the credit gate, which spends money.
			name:    "no evidence at all defaults to subscription",
			profile: nil,
			usage:   UsageShape{},
			want:    KindSubscription,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.profile, tc.usage, tc.isAPIKey); got != tc.want {
				t.Fatalf("Classify() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestKindString(t *testing.T) {
	for k, want := range map[Kind]string{
		KindSubscription: "subscription",
		KindCredit:       "credit",
		KindAPIKey:       "api-key",
	} {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d).String() = %q, want %q", k, got, want)
		}
	}
}

// String and ParseKind are the two halves of how an account's kind survives a
// restart. Keeping the round trip in one test means renaming a spelling on one
// side cannot quietly turn every persisted account of that kind back into a
// subscription.
func TestKindRoundTrip(t *testing.T) {
	for _, k := range []Kind{KindSubscription, KindCredit, KindAPIKey} {
		if got := ParseKind(k.String()); got != k {
			t.Errorf("ParseKind(%q) = %v, want %v", k.String(), got, k)
		}
	}
}

func TestParseKind(t *testing.T) {
	cases := map[string]Kind{
		"credit":   KindCredit,
		"CREDIT":   KindCredit,
		" credit ": KindCredit,
		"api-key":  KindAPIKey,

		"subscription": KindSubscription,
		// An empty name is what an accounts.toml written before the field
		// existed yields; a nonsense name is a typo or a future kind. Both
		// read as subscription, the side that does not spend money.
		"":         KindSubscription,
		"nonsense": KindSubscription,
	}
	for name, want := range cases {
		if got := ParseKind(name); got != want {
			t.Errorf("ParseKind(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestAnEnterpriseUsageBasedSeatClassifiesAsCreditFromTheProfileAlone pins the
// only rule `ccdad add` can apply. That path never calls the usage endpoint —
// it hands Classify an empty UsageShape by construction — so the profile is the
// whole of the evidence, and getting it wrong is permanent rather than
// temporary.
//
// Every value here is VERBATIM from a live claude_enterprise seat read on
// 2026-08-26, and billing_type is the trap the old rule walked into:
// "stripe_subscription_contracted" is one of the four values Claude Code's own
// QG() treats as a SUBSCRIPTION, so no billing_type allowlist can ever reach
// this account no matter what is added to it.
func TestAnEnterpriseUsageBasedSeatClassifiesAsCreditFromTheProfileAlone(t *testing.T) {
	p := &Profile{
		OrganizationType: "claude_enterprise",
		RateLimitTier:    "default_claude_zero",
		SeatTier:         "enterprise_usage_based",
		BillingType:      "stripe_subscription_contracted",
		HasExtraUsage:    true,
	}
	if got := Classify(p, UsageShape{}, false); got != KindCredit {
		t.Fatalf("Classify() = %v, want %v — an add with no usage call has only this profile to go on", got, KindCredit)
	}
}

// TestAnEnterpriseUsageBasedSeatIsCreditEvenOnAnOrdinaryRateLimitTier covers
// the SECOND arm of noPlanWindowProfile on its own.
//
// It is deliberately built so the first arm cannot answer it: rate_limit_tier
// is a perfectly ordinary value here, and only the organization_type/seat_tier
// pair says the seat is metered in money. Without this the second arm is
// unreachable from any test, which is how it was written — a mutation removing
// it left the whole tree green.
//
// NOT MEASURED. Every live capture in hand carries default_claude_zero as well,
// so this pins the rule Claude Code states rather than a reading anyone has
// taken. A seat that is enterprise_usage_based on a non-zero tier may not
// exist; if one is ever captured, this is the test that already covers it.
func TestAnEnterpriseUsageBasedSeatIsCreditEvenOnAnOrdinaryRateLimitTier(t *testing.T) {
	p := &Profile{
		OrganizationType: "claude_enterprise",
		RateLimitTier:    "default_claude_max_20x",
		SeatTier:         "enterprise_usage_based",
		BillingType:      "stripe_subscription_contracted",
	}
	if got := Classify(p, UsageShape{}, false); got != KindCredit {
		t.Fatalf("Classify() = %v, want %v — seat_tier alone names a money-metered seat", got, KindCredit)
	}
}

// TestAnEnterpriseSeatOnAnOrdinaryTierIsNotCreditWithoutTheSeatTier is the
// other half of the pair above, and it is what stops noPlanWindowProfile's
// second arm from widening into "every enterprise organization".
//
// A contracted enterprise org whose seats DO carry plan windows is a
// subscription, and filing it as credit would rank it behind every real
// subscription for no reason. organization_type alone must not decide.
func TestAnEnterpriseSeatOnAnOrdinaryTierIsNotCreditWithoutTheSeatTier(t *testing.T) {
	p := &Profile{
		OrganizationType: "claude_enterprise",
		RateLimitTier:    "default_claude_max_20x",
		SeatTier:         "standard",
		BillingType:      "stripe_subscription_contracted",
	}
	if got := Classify(p, UsageShape{}, false); got != KindSubscription {
		t.Fatalf("Classify() = %v, want %v — an enterprise org is not by itself a money-metered seat", got, KindSubscription)
	}
}
