package oauth

import (
	"fmt"
	"net/http"
)

// tone picks a callback page's accent color.
type tone int

const (
	toneSuccess tone = iota
	toneWarning
	toneDanger
)

func (t tone) colors() (dark, light string) {
	switch t {
	case toneSuccess:
		return "#A6E3A1", "#40A02B"
	case toneWarning:
		return "#F9E2AF", "#DF8E1D"
	default:
		return "#F38BA8", "#D20F39"
	}
}

// page is one browser-facing response.
//
// Every string in it is a compile-time literal. OAuth error text from the query
// is untrusted input and is NEVER interpolated here — reflecting it would put
// attacker-controlled markup on a page served from localhost.
type page struct {
	tone      tone
	title     string
	detail    string
	autoClose bool
}

// writePage renders a small self-contained page. Everything is inline, so the
// page makes no network requests of its own.
func writePage(w http.ResponseWriter, status int, p page) {
	accent, accentLight := p.tone.colors()
	script := ""
	if p.autoClose {
		// Let the page paint before trying to close. Browsers usually refuse to
		// close a tab a script did not open, so the copy always covers closing
		// it by hand.
		script = `<script>setTimeout(function(){window.close()},900)</script>`
	}
	html := fmt.Sprintf(`<!doctype html><html lang="en"><head><meta charset="utf-8">`+
		`<meta name="viewport" content="width=device-width,initial-scale=1"><title>ccdad</title><style>`+
		`:root{--bg:#1E1E2E;--raised:#181825;--line:#313244;--text:#CDD6F4;--dim:#A6ADC8;--faint:#7F849C;--tone:%s}`+
		`@media(prefers-color-scheme:light){:root{--bg:#EFF1F5;--raised:#FFF;--line:#CCD0DA;--text:#1E1E2E;--dim:#6C6F85;--faint:#9CA0B0;--tone:%s}}`+
		`*{box-sizing:border-box;margin:0;padding:0}`+
		`body{font-family:ui-sans-serif,system-ui,"Segoe UI",sans-serif;background:var(--bg);color:var(--text);`+
		`min-height:100vh;display:flex;align-items:center;justify-content:center;padding:24px}`+
		`main{background:var(--raised);border:1px solid var(--line);border-left:3px solid var(--tone);`+
		`border-radius:2px;padding:32px 40px;max-width:440px}`+
		`.eyebrow{font-size:11px;letter-spacing:.08em;text-transform:uppercase;color:var(--faint);margin-bottom:12px}`+
		`h1{font-size:22px;font-weight:550;margin-bottom:8px}`+
		`p{font-size:14px;line-height:1.55;color:var(--dim)}`+
		`</style></head><body><main><div class="eyebrow">ccdad</div><h1>%s</h1><p>%s</p></main>%s</body></html>`,
		accent, accentLight, p.title, p.detail, script)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// The URL in the address bar carries the authorization code; nothing on
	// this page should ever carry it off the machine.
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(html))
}

var (
	pageSuccess = page{
		tone:      toneSuccess,
		title:     "You're logged in",
		detail:    "ccdad captured the login. This tab will try to close itself; if it sticks around, close it and head back to the terminal.",
		autoClose: true,
	}
	pageDeclined = page{
		tone:   toneWarning,
		title:  "Login canceled",
		detail: "You declined the authorization request, so no login was captured. Close this tab; you can retry from ccdad any time.",
	}
	pageFailed = page{
		tone:   toneDanger,
		title:  "Login failed",
		detail: "Claude reported an error during authorization. Close this tab and retry the login from ccdad.",
	}
	pageBlocked = page{
		tone:   toneDanger,
		title:  "Login blocked",
		detail: "This callback didn't match the login ccdad started, so it was rejected for safety. Retry the login from ccdad.",
	}
	pageNoCode = page{
		tone:   toneDanger,
		title:  "No code in the callback",
		detail: "The redirect arrived without an authorization code. Close this tab; ccdad is still waiting for the real callback.",
	}
	pageNotFound = page{
		tone:   toneWarning,
		title:  "Nothing at this address",
		detail: "The login callback arrives at /callback on its own. You can close this tab.",
	}
)
