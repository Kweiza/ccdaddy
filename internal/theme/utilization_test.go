package theme

import "testing"

func TestUtilizationUsesFiveVisibleBands(t *testing.T) {
	for _, tc := range []struct {
		pct  float64
		want Role
	}{
		{0, RoleGaugeCool},
		{19, RoleGaugeCool},
		{20, RoleGaugeOK},
		{39, RoleGaugeOK},
		{40, RoleGaugeWarn},
		{59, RoleGaugeWarn},
		{60, RoleGaugeHigh},
		{79, RoleGaugeHigh},
		{80, RoleGaugeOver},
		{100, RoleGaugeOver},
	} {
		if got := UtilizationRole(tc.pct); got != tc.want {
			t.Errorf("UtilizationRole(%v) = %v, want %v", tc.pct, got, tc.want)
		}
	}
}
