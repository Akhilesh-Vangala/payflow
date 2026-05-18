package checkout

import (
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/Akhilesh-Vangala/payflow/internal/httpx"
	"github.com/Akhilesh-Vangala/payflow/internal/idempotency"
	"github.com/Akhilesh-Vangala/payflow/internal/payments"
	"github.com/Akhilesh-Vangala/payflow/internal/ratelimit"
)

//go:embed all:static
var assets embed.FS

var pageTmpl = template.Must(template.ParseFS(assets, "static/pay.html"))

type Handler struct {
	svc *Service
	// limiter may be nil in tests and when Redis is unavailable at startup; the
	// Limiter's own nil check then applies each rule's FailOpen setting.
	limiter *ratelimit.Limiter
	// vaultURL is the origin the card iframe is served from. It is injected into
	// the page rather than hardcoded in the JavaScript because it is the one
	// value that decides where card numbers go — worth having in exactly one
	// place, set by configuration.
	vaultURL string
	// publicURL is this service's own origin, used to build the link a merchant
	// copies out of the dashboard.
	publicURL string
}

func NewHandler(svc *Service, limiter *ratelimit.Limiter, vaultURL, publicURL string) *Handler {
	return &Handler{
		svc:       svc,
		limiter:   limiter,
		vaultURL:  strings.TrimRight(vaultURL, "/"),
		publicURL: strings.TrimRight(publicURL, "/"),
	}
}

// PublicRoutes are unauthenticated by design — a customer paying a link has no
// account here and never will. See the package comment for what that costs.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Get("/pay/{code}", h.page)
	r.Get("/pay/{code}/info", h.info)
	r.Get("/pay/static/pay.js", h.script)

	// Two budgets on session creation, and they catch different attackers.
	//
	// Per IP stops a script. Per LINK stops a botnet: a distributed attack from
	// a thousand addresses still concentrates on one merchant's link, so the
	// per-IP budget never trips while the per-link one does. Neither alone is
	// sufficient, which is why both are here.
	r.With(
		ratelimit.Middleware(h.limiter, ratelimit.Confirm, ratelimit.ByIP),
		ratelimit.Middleware(h.limiter, ratelimit.ConfirmPerLink, func(req *http.Request) string {
			// Available because route-level middleware runs after chi has
			// matched the pattern and populated the route context.
			return chi.URLParam(req, "code")
		}),
	).Post("/pay/{code}/session", h.startSession)

	r.With(
		ratelimit.Middleware(h.limiter, ratelimit.Confirm, ratelimit.ByIP),
	).Post("/pay/session/{id}/confirm", h.confirm)
}

// SessionRoutes manage links. A person, not an API key: creating a link is
// account configuration, not an API operation.
func (h *Handler) SessionRoutes(r chi.Router) {
	r.Get("/v1/payment_links", h.listLinks)
	r.Post("/v1/payment_links", h.createLink)
	r.Post("/v1/payment_links/{id}/deactivate", h.deactivate)
	r.Post("/v1/payment_links/{id}/activate", h.activate)
}

// --- public ----------------------------------------------------------------

func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	link, err := h.svc.PublicLink(r.Context(), chi.URLParam(r, "code"))
	if errors.Is(err, ErrLinkNotFound) {
		http.Error(w, "This payment link is not available.", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("checkout: load page", "error", err, "trace_id", httpx.TraceID(r.Context()))
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}

	// This page frames the vault. It must not itself be frameable, or a
	// malicious site could overlay it and harvest clicks on a real pay button.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// Every value here goes through html/template's contextual escaping. The
	// name and description are merchant-authored text rendered to strangers, so
	// this is the boundary where a merchant could otherwise script other
	// people's browsers.
	if err := pageTmpl.Execute(w, map[string]any{
		"Link":     link,
		"VaultURL": h.vaultURL,
		"Amount":   formatAmount(link.AmountCents, link.Currency),
	}); err != nil {
		slog.Error("checkout: render page", "error", err)
	}
}

func (h *Handler) script(w http.ResponseWriter, r *http.Request) {
	body, err := assets.ReadFile("static/pay.js")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

func (h *Handler) info(w http.ResponseWriter, r *http.Request) {
	link, err := h.svc.PublicLink(r.Context(), chi.URLParam(r, "code"))
	if errors.Is(err, ErrLinkNotFound) {
		httpx.Fail(w, http.StatusNotFound, httpx.TypeInvalidRequest,
			"resource_missing", "This payment link is not available.")
		return
	}
	if err != nil {
		h.fail(w, r, "link info", err)
		return
	}
	httpx.JSON(w, http.StatusOK, link)
}

func (h *Handler) startSession(w http.ResponseWriter, r *http.Request) {
	cs, err := h.svc.StartSession(r.Context(), chi.URLParam(r, "code"), clientIP(r), r.UserAgent())
	if errors.Is(err, ErrLinkNotFound) {
		httpx.Fail(w, http.StatusNotFound, httpx.TypeInvalidRequest,
			"resource_missing", "This payment link is not available.")
		return
	}
	if err != nil {
		h.fail(w, r, "start session", err)
		return
	}
	httpx.JSON(w, http.StatusCreated, cs)
}

func (h *Handler) confirm(w http.ResponseWriter, r *http.Request) {
	sessionID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_id", "The checkout session ID is not valid.")
		return
	}

	var req struct {
		PaymentToken string `json:"payment_token"`
		DeviceID     string `json:"device_id"`
		// There is deliberately no amount field. See the package comment.
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_json", "Request body could not be parsed as JSON.")
		return
	}
	if req.PaymentToken == "" {
		httpx.FailParam(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"missing_payment_token", "A payment token is required.", "payment_token")
		return
	}

	result, err := h.svc.Confirm(r.Context(), ConfirmInput{
		SessionID: sessionID, PaymentToken: req.PaymentToken,
		IP: clientIP(r), DeviceID: req.DeviceID, UserAgent: r.UserAgent(),
	})
	switch {
	case errors.Is(err, ErrSessionNotFound):
		// Worth a log line, not just a 404. A stream of these is someone
		// guessing session ids, which is not something a real customer does.
		h.reject(r, "confirm", "session_not_found", "checkout_session_id", sessionID)
		httpx.Fail(w, http.StatusNotFound, httpx.TypeInvalidRequest,
			"resource_missing", "This checkout session does not exist.")
		return
	case errors.Is(err, ErrSessionUsed):
		// 409 rather than 400: the client's correct response is to start a new
		// session, which the page does automatically.
		h.reject(r, "confirm", "session_already_used", "checkout_session_id", sessionID)
		httpx.Fail(w, http.StatusConflict, httpx.TypeInvalidRequest,
			"session_used", "This checkout session has already been used.")
		return
	case errors.Is(err, ErrSessionExpired):
		h.reject(r, "confirm", "session_expired", "checkout_session_id", sessionID)
		httpx.Fail(w, http.StatusConflict, httpx.TypeInvalidRequest,
			"session_expired", "This checkout session expired. Please try again.")
		return
	case errors.Is(err, idempotency.ErrInFlight):
		// A concurrent confirm for this same session is still running — a
		// double-clicked pay button. Tell the client to wait, NOT to start a new
		// session: a new session means a new idempotency key, and a new key on a
		// card that is already being charged is how you charge someone twice.
		httpx.Fail(w, http.StatusConflict, httpx.TypeIdempotency, "request_in_flight",
			"This payment is already being processed. Please wait a moment.")
		return
	case errors.Is(err, payments.ErrTokenUnusable):
		// The single most useful line on this page for spotting abuse. A card
		// tester tokenizes directly against the vault and replays tokens; a real
		// customer produces this at most once, by fumbling a form.
		h.reject(r, "confirm", "payment_token_unusable", "checkout_session_id", sessionID)
		httpx.FailParam(w, http.StatusBadRequest, httpx.TypeCard,
			"invalid_payment_token",
			"The card details could not be used. Please re-enter them.", "payment_token")
		return
	case err != nil:
		h.fail(w, r, "confirm", err, "checkout_session_id", sessionID)
		return
	}

	// Logged for every outcome, including success. A declined payment on a link
	// is not a server error and would otherwise leave no trace at all — and
	// "declines on this link went from 2% to 90%" is how card testing is
	// actually noticed.
	slog.Info("checkout: confirmed",
		"checkout_session_id", sessionID,
		"charge_id", result.ChargeID,
		"outcome", result.Status,
		"reason", result.Message,
		"trace_id", httpx.TraceID(r.Context()),
		"ip", clientIP(r),
	)
	httpx.JSON(w, http.StatusOK, result)
}

// --- dashboard -------------------------------------------------------------

func (h *Handler) listLinks(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := httpx.MerchantID(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, httpx.TypeAuthentication,
			"missing_merchant", "Could not resolve a merchant for this request.")
		return
	}
	links, err := h.svc.ListLinks(r.Context(), merchantID)
	if err != nil {
		h.fail(w, r, "list links", err)
		return
	}
	for i := range links {
		links[i].URL = h.publicURL + "/pay/" + links[i].Code
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"object": "list", "data": links})
}

func (h *Handler) createLink(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := httpx.MerchantID(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, httpx.TypeAuthentication,
			"missing_merchant", "Could not resolve a merchant for this request.")
		return
	}
	userID, _ := httpx.UserID(r.Context())

	var req struct {
		Amount      int64  `json:"amount"`
		Currency    string `json:"currency"`
		Name        string `json:"name"`
		Description string `json:"description"`
		SuccessURL  string `json:"success_url"`
		CancelURL   string `json:"cancel_url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_json", "Request body could not be parsed as JSON.")
		return
	}
	if req.Currency == "" {
		req.Currency = "USD"
	}

	link, err := h.svc.CreateLink(r.Context(), merchantID, &userID, LinkInput{
		AmountCents: req.Amount, Currency: req.Currency, Name: req.Name,
		Description: req.Description, SuccessURL: req.SuccessURL, CancelURL: req.CancelURL,
	})
	switch {
	case errors.Is(err, ErrInvalidAmount):
		httpx.FailParam(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_amount", "Amount must be a positive integer in minor units.", "amount")
		return
	case errors.Is(err, ErrInvalidURL):
		httpx.FailParam(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_url", "Redirect URLs must start with http:// or https://.", "success_url")
		return
	case err != nil && strings.Contains(err.Error(), "required"):
		httpx.FailParam(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_name", "A name is required.", "name")
		return
	case err != nil:
		h.fail(w, r, "create link", err)
		return
	}
	link.URL = h.publicURL + "/pay/" + link.Code
	httpx.JSON(w, http.StatusCreated, link)
}

func (h *Handler) deactivate(w http.ResponseWriter, r *http.Request) { h.setActive(w, r, false) }
func (h *Handler) activate(w http.ResponseWriter, r *http.Request)   { h.setActive(w, r, true) }

func (h *Handler) setActive(w http.ResponseWriter, r *http.Request, active bool) {
	merchantID, ok := httpx.MerchantID(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, httpx.TypeAuthentication,
			"missing_merchant", "Could not resolve a merchant for this request.")
		return
	}
	linkID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_id", "The link ID is not a valid UUID.")
		return
	}
	switch err := h.svc.SetActive(r.Context(), merchantID, linkID, active); {
	case errors.Is(err, ErrLinkNotFound):
		httpx.Fail(w, http.StatusNotFound, httpx.TypeInvalidRequest,
			"resource_missing", "No such payment link.")
	case err != nil:
		h.fail(w, r, "set link active", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// fail is the single place an unexpected checkout error becomes a response.
//
// The two halves say deliberately different things. The CALLER gets
// "An unexpected error occurred." — a database error's text leaks schema and
// query shape, and this endpoint is public. The LOG gets everything: the
// wrapped error chain (every layer wraps with %w and names its operation, so
// the message reads as a path), the trace id that ties it to the request line,
// and whichever identifiers the caller passed in `fields`.
//
// The rule this enforces: no error is ever discarded or flattened to "failed".
// If you cannot tell from one log line which operation broke and on which
// record, the line is not finished.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, op string, err error, fields ...any) {
	args := append([]any{
		"error", err,
		"op", op,
		"path", r.URL.Path,
		"trace_id", httpx.TraceID(r.Context()),
		"ip", clientIP(r),
	}, fields...)
	slog.Error("checkout: "+op, args...)
	httpx.Fail(w, http.StatusInternalServerError, httpx.TypeAPI,
		"internal_error", "An unexpected error occurred.")
}

// reject logs an expected refusal — a bad link, a used session, a declined
// card. These are not server faults, so they are Warn rather than Error, but
// they are the lines that matter most when reading logs after the fact: a burst
// of them against one link is a card-testing attack, and a system that only
// logs its own crashes cannot show you that.
func (h *Handler) reject(r *http.Request, op, reason string, fields ...any) {
	args := append([]any{
		"op", op,
		"reason", reason,
		"path", r.URL.Path,
		"trace_id", httpx.TraceID(r.Context()),
		"ip", clientIP(r),
		"user_agent", r.UserAgent(),
	}, fields...)
	slog.Warn("checkout: rejected", args...)
}

// formatAmount renders minor units for display only. Integer arithmetic
// throughout — the string is built from the digits, never from a float, so this
// cannot introduce the rounding the money package exists to prevent.
func formatAmount(cents int64, currency string) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	major, minor := cents/100, cents%100
	return sign + itoaGrouped(major) + "." + string(rune('0'+minor/10)) + string(rune('0'+minor%10)) + " " + currency
}

func itoaGrouped(n int64) string {
	s := []byte{}
	if n == 0 {
		return "0"
	}
	for i := 0; n > 0; i++ {
		if i > 0 && i%3 == 0 {
			s = append([]byte{','}, s...)
		}
		s = append([]byte{byte('0' + n%10)}, s...)
		n /= 10
	}
	return string(s)
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if first, _, ok := strings.Cut(fwd, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(fwd)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
