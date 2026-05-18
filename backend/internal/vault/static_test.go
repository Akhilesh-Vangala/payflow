package vault_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Akhilesh-Vangala/payflow/internal/vault"
)

func checkoutServer(t *testing.T) http.Handler {
	t.Helper()
	h, err := vault.CheckoutHandler([]string{"http://localhost:3000"})
	if err != nil {
		t.Fatalf("checkout handler: %v", err)
	}
	return http.StripPrefix("/checkout", h)
}

func fetch(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	checkoutServer(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// Regression: the card frame silently stopped working because its script was
// inline while the CSP says script-src 'self'. Nothing errored — the page
// loaded, the JavaScript never ran, and tokenization hung until it timed out.
//
// Adding 'unsafe-inline' would have "fixed" it and quietly removed the control
// that matters most here: this is the one document in the system that can see
// a card number, so an injected inline script could exfiltrate a PAN.
func TestCardFrameHasNoInlineScript(t *testing.T) {
	rec := fetch(t, "/checkout/iframe.html")
	if rec.Code != http.StatusOK {
		t.Fatalf("iframe.html returned %d", rec.Code)
	}

	body := rec.Body.String()
	if strings.Contains(body, `<script type="module">`) || strings.Contains(body, "<script>") {
		t.Error("iframe.html contains an inline script, which its own CSP blocks — " +
			"move it to a file rather than relaxing script-src")
	}
	if !strings.Contains(body, `src="./checkout.js"`) {
		t.Error("iframe.html does not load its script from a file")
	}
}

// The CSP must keep forbidding inline execution.
func TestCardFrameCSPForbidsInlineScripts(t *testing.T) {
	csp := fetch(t, "/checkout/iframe.html").Header().Get("Content-Security-Policy")

	if !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("script-src is not 'self': %q", csp)
	}
	if strings.Contains(csp, "'unsafe-inline'") && strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Error("script-src allows unsafe-inline; an injected script here could read a PAN")
	}
	// connect-src 'self' is what stops an injected script posting a card
	// number to an attacker's server.
	if !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("connect-src is not restricted to 'self': %q", csp)
	}
	// The frame is meant to be embedded, so frame-ancestors must name the
	// merchants rather than deny outright.
	if !strings.Contains(csp, "frame-ancestors http://localhost:3000") {
		t.Errorf("frame-ancestors does not allow the configured origin: %q", csp)
	}
}

// Every asset the frame references must actually be served, or the frame is
// broken in exactly the way that is hardest to notice.
func TestCheckoutAssetsAreServed(t *testing.T) {
	for _, asset := range []string{
		"/checkout/iframe.html",
		"/checkout/checkout.js",
		"/checkout/card.js",
		"/checkout/payflow.js",
	} {
		rec := fetch(t, asset)
		if rec.Code != http.StatusOK {
			t.Errorf("%s returned %d, want 200", asset, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s is empty", asset)
		}
	}
}

// The frame must never be cached to disk on a shared machine.
func TestCardFrameIsNotCached(t *testing.T) {
	cc := fetch(t, "/checkout/iframe.html").Header().Get("Cache-Control")
	if !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// With no configured origins the frame must refuse to be embedded at all —
// an open frame is worse than a broken demo.
func TestUnconfiguredOriginsDenyFraming(t *testing.T) {
	h, err := vault.CheckoutHandler(nil)
	if err != nil {
		t.Fatalf("checkout handler: %v", err)
	}
	rec := httptest.NewRecorder()
	http.StripPrefix("/checkout", h).ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/checkout/iframe.html", nil))

	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Error("with no allowed origins, frame-ancestors must be 'none'")
	}
}
