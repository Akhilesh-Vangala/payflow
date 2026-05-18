package accounts

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/Akhilesh-Vangala/payflow/internal/httpx"
	"github.com/Akhilesh-Vangala/payflow/internal/ratelimit"
)

type Handler struct {
	svc     *Service
	limiter *ratelimit.Limiter
}

func NewHandler(svc *Service, limiter *ratelimit.Limiter) *Handler {
	return &Handler{svc: svc, limiter: limiter}
}

// PublicRoutes are reachable without credentials — they are how credentials are
// obtained in the first place.
//
// Both are rate limited and both FAIL CLOSED. If Redis is unreachable these
// endpoints stop answering, which is the deliberate choice: an attacker who can
// knock out the cache would otherwise get unlimited password attempts, and a
// brief inability to sign in is recoverable in a way a guessed password is not.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.With(
		ratelimit.Middleware(h.limiter, ratelimit.Register, ratelimit.ByIP),
	).Post("/v1/auth/register", h.register)

	r.With(
		ratelimit.Middleware(h.limiter, ratelimit.Login, ratelimit.ByIP),
	).Post("/v1/auth/login", h.login)
}

// SessionRoutes require a signed-in person, not an API key. Mount behind
// httpx.Authenticate + httpx.RequireSession.
func (h *Handler) SessionRoutes(r chi.Router) {
	r.Post("/v1/auth/logout", h.logout)
	r.Get("/v1/account", h.me)
	r.Get("/v1/account/api_keys", h.listKeys)
	r.Post("/v1/account/api_keys", h.createKey)
	r.Post("/v1/account/api_keys/{id}/revoke", h.revokeKey)
}

type registerRequest struct {
	BusinessName string `json:"business_name"`
	Email        string `json:"email"`
	Password     string `json:"password"`
	Name         string `json:"name"`
	Country      string `json:"country"`
	Currency     string `json:"currency"`
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !decode(w, r, &req) {
		return
	}

	session, apiKey, err := h.svc.Register(r.Context(), RegisterInput{
		BusinessName: req.BusinessName, Email: req.Email, Password: req.Password,
		Name: req.Name, Country: req.Country, Currency: req.Currency,
		IP: clientIP(r), UserAgent: r.UserAgent(),
	})
	switch {
	case errors.Is(err, ErrEmailTaken):
		// Registration is the one endpoint where existence genuinely cannot be
		// hidden — the address either can or cannot be used. Telling the person
		// plainly beats a fake success that leaves them unable to sign in.
		httpx.FailParam(w, http.StatusConflict, httpx.TypeInvalidRequest,
			"email_taken", "An account already exists for that email address.", "email")
		return
	case errors.Is(err, ErrWeakPassword):
		httpx.FailParam(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"weak_password", "Password must be at least 10 characters.", "password")
		return
	case errors.Is(err, ErrInvalidEmail):
		httpx.FailParam(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_email", "That is not a valid email address.", "email")
		return
	case err != nil:
		slog.Error("accounts: register",
			"error", err, "op", "register",
			"email", strings.ToLower(strings.TrimSpace(req.Email)),
			"ip", clientIP(r), "trace_id", httpx.TraceID(r.Context()))
		httpx.Fail(w, http.StatusInternalServerError, httpx.TypeAPI,
			"internal_error", "An unexpected error occurred.")
		return
	}

	slog.Info("accounts: registered",
		"merchant_id", session.User.MerchantID, "user_id", session.User.ID,
		"email", session.User.Email,
		"ip", clientIP(r), "user_agent", r.UserAgent(),
		"trace_id", httpx.TraceID(r.Context()))

	// The API key appears here and nowhere else, ever.
	httpx.JSON(w, http.StatusCreated, map[string]any{
		"session": session,
		"api_key": apiKey,
	})
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}

	session, err := h.svc.Login(r.Context(), req.Email, req.Password, clientIP(r), r.UserAgent())
	if errors.Is(err, ErrInvalidLogin) {
		// A failed login is the single most important security event this
		// service produces, and until now it left no trace at all — which means
		// a password-guessing run against a merchant account would have been
		// completely invisible in the logs.
		//
		// The RESPONSE still refuses to say whether the address exists; the LOG
		// is on our side of that boundary and records everything needed to
		// investigate. Note what is absent: the password, and any hint of
		// whether it was the address or the password that was wrong — the log
		// would otherwise become the enumeration oracle the endpoint is not.
		slog.Warn("accounts: login failed",
			"reason", "invalid_credentials",
			"email", strings.ToLower(strings.TrimSpace(req.Email)),
			"ip", clientIP(r),
			"user_agent", r.UserAgent(),
			"trace_id", httpx.TraceID(r.Context()))
		httpx.Fail(w, http.StatusUnauthorized, httpx.TypeAuthentication,
			"invalid_credentials", "That email and password combination is not correct.")
		return
	}
	if err != nil {
		slog.Error("accounts: login",
			"error", err,
			"op", "login",
			"ip", clientIP(r),
			"trace_id", httpx.TraceID(r.Context()))
		httpx.Fail(w, http.StatusInternalServerError, httpx.TypeAPI,
			"internal_error", "An unexpected error occurred.")
		return
	}

	// Successful logins are logged too. A brute force is only visible as a
	// pattern, and "300 failures then one success from the same IP" is the
	// pattern that matters — you cannot see it if you only log the failures.
	slog.Info("accounts: login",
		"user_id", session.User.ID,
		"merchant_id", session.User.MerchantID,
		"ip", clientIP(r),
		"user_agent", r.UserAgent(),
		"trace_id", httpx.TraceID(r.Context()))
	httpx.JSON(w, http.StatusOK, session)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if err := h.svc.Logout(r.Context(), strings.TrimSpace(raw)); err != nil {
		slog.Error("accounts: logout", "error", err, "trace_id", httpx.TraceID(r.Context()))
	}
	// Always 204. A logout that can fail visibly encourages clients to keep the
	// token when it does.
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	userID, ok := httpx.UserID(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, httpx.TypeAuthentication,
			"invalid_session", "Please sign in again.")
		return
	}
	user, err := h.svc.Me(r.Context(), userID)
	if err != nil {
		slog.Error("accounts: me", "error", err, "trace_id", httpx.TraceID(r.Context()))
		httpx.Fail(w, http.StatusInternalServerError, httpx.TypeAPI,
			"internal_error", "An unexpected error occurred.")
		return
	}
	httpx.JSON(w, http.StatusOK, user)
}

func (h *Handler) listKeys(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := httpx.MerchantID(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, httpx.TypeAuthentication,
			"invalid_session", "Please sign in again.")
		return
	}
	keys, err := h.svc.ListKeys(r.Context(), merchantID)
	if err != nil {
		slog.Error("accounts: list keys", "error", err, "trace_id", httpx.TraceID(r.Context()))
		httpx.Fail(w, http.StatusInternalServerError, httpx.TypeAPI,
			"internal_error", "An unexpected error occurred.")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"object": "list", "data": keys})
}

func (h *Handler) createKey(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := httpx.MerchantID(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, httpx.TypeAuthentication,
			"invalid_session", "Please sign in again.")
		return
	}
	userID, _ := httpx.UserID(r.Context())

	var req struct {
		Name  string `json:"name"`
		Mode  string `json:"mode"`
		Scope string `json:"scope"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Mode == "" {
		req.Mode = "test"
	}
	if req.Scope == "" {
		req.Scope = "write"
	}
	// Live keys move real money. This build has no sponsor bank and no
	// onboarding review, so there is nothing a live key could legitimately do —
	// issuing one would be a lie about what the account is capable of.
	if req.Mode == "live" {
		httpx.FailParam(w, http.StatusForbidden, httpx.TypeInvalidRequest,
			"live_mode_unavailable",
			"Live keys require completed onboarding and a sponsor bank, which this "+
				"deployment does not have. Use a test key.", "mode")
		return
	}

	raw, err := h.svc.CreateKey(r.Context(), merchantID, &userID, req.Name, req.Mode, req.Scope)
	if err != nil {
		slog.Error("accounts: create key",
			"error", err, "op", "create_key",
			"merchant_id", merchantID, "user_id", userID,
			"mode", req.Mode, "scope", req.Scope,
			"trace_id", httpx.TraceID(r.Context()))
		httpx.Fail(w, http.StatusInternalServerError, httpx.TypeAPI,
			"internal_error", "An unexpected error occurred.")
		return
	}

	// An audit line, not a debug line. Minting a credential that can charge
	// cards is exactly the action a merchant will later need to prove happened —
	// or prove did not. The key PREFIX only; the key itself is never logged
	// anywhere, by anyone, ever (§13).
	slog.Info("accounts: api key created",
		"merchant_id", merchantID, "user_id", userID,
		"prefix", raw[:16], "mode", req.Mode, "scope", req.Scope,
		"name", req.Name,
		"ip", clientIP(r), "trace_id", httpx.TraceID(r.Context()))
	httpx.JSON(w, http.StatusCreated, map[string]any{
		"key": raw,
		"warning": "This is the only time this key will be shown. Store it in your " +
			"server's secret manager now.",
	})
}

func (h *Handler) revokeKey(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := httpx.MerchantID(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, httpx.TypeAuthentication,
			"invalid_session", "Please sign in again.")
		return
	}
	keyID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_id", "The key ID is not a valid UUID.")
		return
	}

	switch err := h.svc.RevokeKey(r.Context(), merchantID, keyID); {
	case errors.Is(err, ErrKeyNotFound):
		httpx.Fail(w, http.StatusNotFound, httpx.TypeInvalidRequest,
			"resource_missing", "No such API key.")
	case err != nil:
		slog.Error("accounts: revoke key",
			"error", err, "op", "revoke_key",
			"merchant_id", merchantID, "api_key_id", keyID,
			"trace_id", httpx.TraceID(r.Context()))
		httpx.Fail(w, http.StatusInternalServerError, httpx.TypeAPI,
			"internal_error", "An unexpected error occurred.")
	default:
		// The other half of the credential audit trail. Revocation is the action
		// taken during an incident, so its timestamp is evidence.
		slog.Info("accounts: api key revoked",
			"merchant_id", merchantID, "api_key_id", keyID,
			"ip", clientIP(r), "trace_id", httpx.TraceID(r.Context()))
		w.WriteHeader(http.StatusNoContent)
	}
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(dst); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_json", "Request body could not be parsed as JSON.")
		return false
	}
	return true
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
