package httpx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Two credentials reach this API and they are not interchangeable.
//
//	an API KEY  authenticates a merchant's SERVER  — long-lived, machine-held
//	a SESSION   authenticates a PERSON             — short-lived, browser-held
//
// Both resolve to the same merchant, so read endpoints accept either. Account
// management accepts only a session, enforced by RequireSession below. The
// reason is escalation: if an API key could mint an API key, then a leaked
// read-only key is a write key one request later, and revoking the leaked key
// does not revoke what it created.
const (
	PrincipalAPIKey  = "api_key"
	PrincipalSession = "session"
)

const (
	ctxKeyUserID    ctxKey = "user_id"
	ctxKeyPrincipal ctxKey = "principal"
)

// SessionTokenPrefix distinguishes a dashboard session from an API key in the
// Authorization header, so one middleware can route to the right lookup
// without trying both against the database.
const SessionTokenPrefix = "sess_"

func UserID(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(ctxKeyUserID).(uuid.UUID)
	return id, ok
}

func Principal(ctx context.Context) string {
	p, _ := ctx.Value(ctxKeyPrincipal).(string)
	return p
}

// HashSessionToken derives the stored lookup value for a session token.
//
// SHA-256, like an API key and unlike a password: the token is 256 bits of
// CSPRNG output, so there is no dictionary to attack, and the lookup has to be
// indexable.
func HashSessionToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Authenticate resolves either credential to a merchant.
//
// Dispatch is on the token prefix rather than by trying both lookups: a
// mistyped API key must not also be checked against the session table, and one
// query per request is the point of putting a prefix on these values at all.
func Authenticate(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := bearerToken(r)
			if raw == "" {
				Fail(w, http.StatusUnauthorized, TypeAuthentication, "missing_credentials",
					"No credentials provided. Send an API key or a dashboard session "+
						"as 'Authorization: Bearer ...'.")
				return
			}

			if strings.HasPrefix(raw, SessionTokenPrefix) {
				ctx, ok := resolveSession(w, r, pool, raw)
				if !ok {
					return
				}
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			ctx, ok := resolveAPIKey(w, r, pool, raw)
			if !ok {
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireSession rejects an API-key-authenticated request. Mount it inside a
// group that has already run Authenticate.
func RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if Principal(r.Context()) != PrincipalSession {
			// An API key reaching an account-management route is either a
			// merchant misreading the docs or a leaked key attempting to mint
			// itself a better one. Both are worth a line; the second is worth an
			// alert.
			merchantID, _ := MerchantID(r.Context())
			slog.Warn("auth: session required",
				"reason", "api key used on an account-management route",
				"principal", Principal(r.Context()),
				"merchant_id", merchantID,
				"path", r.URL.Path,
				"ip", clientIPFromRequest(r),
				"trace_id", TraceID(r.Context()))
			Fail(w, http.StatusForbidden, TypeAuthentication, "session_required",
				"This endpoint requires a signed-in dashboard session. An API key "+
					"cannot manage account credentials.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIPFromRequest is best-effort and deliberately labelled as such.
//
// X-Forwarded-For is caller-controlled on a request that has not yet been
// authenticated, so a determined attacker can put anything in it. It is good
// enough to correlate log lines and useless as an access-control input — never
// make a security decision on this value.
func clientIPFromRequest(r *http.Request) string {
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

func resolveSession(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, raw string) (context.Context, bool) {
	var userID, merchantID uuid.UUID

	// Expiry is compared against now() inside Postgres, not against a pod
	// clock — a pod running fast would honour a session past its life.
	err := pool.QueryRow(r.Context(), `
		SELECT s.user_id, u.merchant_id
		FROM merchant_sessions s
		JOIN merchant_users u ON u.id = s.user_id
		WHERE s.token_hash = $1
		  AND s.revoked_at IS NULL
		  AND s.expires_at > now()`,
		HashSessionToken(raw),
	).Scan(&userID, &merchantID)

	if errors.Is(err, pgx.ErrNoRows) {
		// One query covers three distinct causes — no such token, revoked, or
		// expired — because distinguishing them in the RESPONSE would tell an
		// attacker whether a guessed token was ever real. The log says as much
		// as it honestly can: the query cannot tell them apart either, so it
		// says so rather than guessing.
		slog.Warn("auth: session rejected",
			"reason", "unknown, revoked, or expired",
			"path", r.URL.Path,
			"ip", clientIPFromRequest(r),
			"trace_id", TraceID(r.Context()))
		Fail(w, http.StatusUnauthorized, TypeAuthentication, "invalid_session",
			"Your session has expired. Please sign in again.")
		return nil, false
	}
	if err != nil {
		slog.Error("auth: session lookup failed", "error", err, "trace_id", TraceID(r.Context()))
		Fail(w, http.StatusInternalServerError, TypeAPI, "internal_error",
			"An unexpected error occurred.")
		return nil, false
	}

	ctx := context.WithValue(r.Context(), ctxKeyMerchantID, merchantID)
	ctx = context.WithValue(ctx, ctxKeyUserID, userID)
	ctx = context.WithValue(ctx, ctxKeyPrincipal, PrincipalSession)
	// A dashboard session is not scoped to test or live. Test is the safe
	// default: nothing reached from a browser should be able to steer the real
	// processor.
	ctx = context.WithValue(ctx, ctxKeyMode, "test")
	return ctx, true
}

// keyCache is process-wide. Package-level rather than threaded through every
// constructor because authentication happens in middleware that already takes
// only a pool, and there is exactly one of these per process by definition.
var keyCache = NewKeyCache()

// InvalidateAPIKey drops a revoked key from this pod's cache immediately.
func InvalidateAPIKey(raw string) { keyCache.Invalidate(HashAPIKey(raw)) }

func resolveAPIKey(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, raw string) (context.Context, bool) {
	var merchantID uuid.UUID
	var mode, scope string

	hash := HashAPIKey(raw)

	if e, hit := keyCache.get(hash); hit {
		if !e.found {
			// A cached rejection. Still logged — a spray of invalid keys is the
			// signal, and it would disappear entirely if the cache short-
			// circuited silently.
			slog.Warn("auth: unknown api key", "prefix", keyPrefix(raw),
				"cached", true, "trace_id", TraceID(r.Context()))
			Fail(w, http.StatusUnauthorized, TypeAuthentication, "invalid_api_key",
				"The provided API key is invalid or has been revoked.")
			return nil, false
		}
		merchantID, mode, scope = e.merchantID, e.mode, e.scope
		return authorized(w, r, merchantID, mode, scope)
	}

	err := pool.QueryRow(r.Context(), `
		SELECT merchant_id, mode, scope
		FROM api_keys
		WHERE key_hash = $1 AND revoked_at IS NULL`,
		hash,
	).Scan(&merchantID, &mode, &scope)

	if errors.Is(err, pgx.ErrNoRows) {
		keyCache.put(hash, cacheEntry{found: false})
		slog.Warn("auth: unknown api key", "prefix", keyPrefix(raw),
			"trace_id", TraceID(r.Context()))
		Fail(w, http.StatusUnauthorized, TypeAuthentication, "invalid_api_key",
			"The provided API key is invalid or has been revoked.")
		return nil, false
	}
	if err != nil {
		// A transient database error is NOT cached. Caching it would turn a
		// one-second blip into a TTL-long outage for that key.
		slog.Error("auth: api key lookup failed", "error", err, "trace_id", TraceID(r.Context()))
		Fail(w, http.StatusInternalServerError, TypeAPI, "internal_error",
			"An unexpected error occurred.")
		return nil, false
	}

	keyCache.put(hash, cacheEntry{merchantID: merchantID, mode: mode, scope: scope, found: true})
	return authorized(w, r, merchantID, mode, scope)
}

// authorized applies scope and builds the request context. Shared by the cached
// and uncached paths so a scope check can never be skipped on a cache hit —
// which would be a silent privilege escalation that only appears under load.
func authorized(w http.ResponseWriter, r *http.Request, merchantID uuid.UUID, mode, scope string) (context.Context, bool) {
	if scope == "read" && r.Method != http.MethodGet {
		Fail(w, http.StatusForbidden, TypeAuthentication, "insufficient_scope",
			"This API key is read-only.")
		return nil, false
	}

	ctx := context.WithValue(r.Context(), ctxKeyMerchantID, merchantID)
	ctx = context.WithValue(ctx, ctxKeyMode, mode)
	ctx = context.WithValue(ctx, ctxKeyPrincipal, PrincipalAPIKey)
	return ctx, true
}
