package release

import "testing"

// The tag is the one field the signature binds a release by, so its grammar is
// the grammar of the trusted comment as well: a request that spells the same
// release two ways must produce one string, or a user's missing `v` fails as
// "wrong release" and routes them to a tamper remedy for a typo.
func TestParseTagNormalizes(t *testing.T) {
	for _, c := range []struct {
		in   string
		want string // Tag(); "" means the input must be refused
	}{
		{"v1.2.3", "v1.2.3"},
		{"1.2.3", "v1.2.3"},
		{"  v1.2.3  ", "v1.2.3"},
		{"\tv1.2.3\n", "v1.2.3"},
		{"v0.7.0", "v0.7.0"},
		{"v1.2.3-rc.1", "v1.2.3-rc.1"},
		{"v01.2.3", "v1.2.3"}, // leading zero is a spelling, not a refusal -- Atoi normalises it
		{"", ""},
		{"v1.2", ""},
		{"1.2.3.4", ""},
		{"vv1.2.3", ""},
		{"v1.2.3+build", ""},
		{"latest", ""},
		{"v1.2.3/../../etc", ""},
		{"v1.2.3 v9.9.9", ""},
		{"v1.2.3-rc.1;rm -rf /", ""},
		{"v1234567890.0.0", ""}, // ten digits: bounded before strconv sees it
	} {
		t.Run(c.in, func(t *testing.T) {
			v, ok := ParseTag(c.in)
			if c.want == "" {
				if ok {
					t.Fatalf("ParseTag(%q) accepted %v; an origin chooses this string", c.in, v)
				}
				return
			}
			if !ok {
				t.Fatalf("ParseTag(%q) refused a legal tag", c.in)
			}
			if got := v.Tag(); got != c.want {
				t.Errorf("Tag() = %q, want %q", got, c.want)
			}
			if got, want := v.String(), c.want[1:]; got != want {
				t.Errorf("String() = %q, want %q — String never carries the v", got, want)
			}
		})
	}
}

// Ordering decides two refusals: "already current" and "the origin answered
// with something older than what you are running".
func TestVersionCompare(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "v1.2.3", 0},
		{"v1.2.4", "v1.2.3", 1},
		{"v1.3.0", "v1.2.9", 1},
		{"v2.0.0", "v1.99.99", 1},
		{"v0.6.1", "v0.7.0", -1},
		{"v0.10.0", "v0.9.0", 1}, // not a string comparison
		{"v1.2.3-rc.1", "v1.2.3", -1},
		{"v1.2.3", "v1.2.3-rc.1", 1},
		{"v1.2.3-rc.1", "v1.2.3-rc.2", -1},
		{"v1.2.3-rc.1", "v1.2.3-rc.1", 0},
	} {
		t.Run(c.a+"_vs_"+c.b, func(t *testing.T) {
			a, ok := ParseTag(c.a)
			if !ok {
				t.Fatalf("ParseTag(%q) refused", c.a)
			}
			b, ok := ParseTag(c.b)
			if !ok {
				t.Fatalf("ParseTag(%q) refused", c.b)
			}
			if got := a.Compare(b); got != c.want {
				t.Errorf("%s.Compare(%s) = %d, want %d", c.a, c.b, got, c.want)
			}
			if got := b.Compare(a); got != -c.want {
				t.Errorf("%s.Compare(%s) = %d, want %d — Compare must be antisymmetric", c.b, c.a, got, -c.want)
			}
		})
	}
}
