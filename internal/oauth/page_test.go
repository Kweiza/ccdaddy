package oauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The success page is the only one that tries to close the tab, and its copy —
// "This tab will try to close itself" — is only true because of the script.
// Flipping autoClose off is green today and turns that sentence into a lie;
// switching it on for any other page would try to close a tab whose copy tells
// the user to close it by hand, and would do it while the login is still
// running, since the declined, failed, blocked and no-code pages all leave the
// listener waiting.
func TestOnlyTheSuccessPageTriesToCloseTheTab(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    page
		want bool
	}{
		{"success", pageSuccess, true},
		{"declined", pageDeclined, false},
		{"failed", pageFailed, false},
		{"blocked", pageBlocked, false},
		{"no code", pageNoCode, false},
		{"not found", pageNotFound, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writePage(rec, http.StatusOK, tc.p)
			body := rec.Body.String()

			if got := strings.Contains(body, "<script"); got != tc.want {
				t.Fatalf("script tag present = %v, want %v", got, tc.want)
			}
			if tc.want && !strings.Contains(body, "window.close()") {
				t.Fatalf("the success page carries a script that does not close the tab: %q", body)
			}
		})
	}
}
