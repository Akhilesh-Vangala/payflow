package httpx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ctxKey string

const (
	ctxKeyMerchantID ctxKey = "merchant_id"
	ctxKeyTraceID    ctxKey = "trace_id"
	ctxKeyMode       ctxKey = "mode"
)

func MerchantID(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(ctxKeyMerchantID).(uuid.UUID)
	return id, ok
}

func TraceID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyTraceID).(string)
	return id
}

func Mode(ctx context.Context) string {
	m, _ := ctx.Value(ctxKeyMode).(string)
	return m
}

// Trace attaches a trace ID to every request and propagates it via context, so
// a single charge can be followed across services in logs (§12).
func Trace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := r.Header.Get("X-Trace-Id")
		if traceID == "" {
			traceID = uuid.NewString()
		}
		w.Header().Set("X-Trace-Id", traceID)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKeyTraceID, traceID)))
	})
}

// LogRequests emits one structured line per request (§12).
func LogRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"trace_id", TraceID(r.Context()),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// HashAPIKey derives the stored lookup value for a raw API key.
//
// Plain SHA-256 rather than bcrypt/argon2, deliberately: API keys are
// high-entropy random values, not user-chosen passwords, so there is no
// dictionary to attack and no need for a slow KDF. Using bcrypt here would
// also make lookup impossible — you cannot index a salted-per-row hash, and
// every request would have to scan the table.
func HashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// AuthenticateAPIKey resolves a bearer API key to a merchant (§8).
//
// Use this where only a machine credential makes sense. The dashboard's mixed
// routes use Authenticate, which also accepts a browser session.
func AuthenticateAPIKey(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := bearerToken(r)
			if raw == "" {
				Fail(w, http.StatusUnauthorized, TypeAuthentication, "missing_api_key",
					"No API key provided. Send it as 'Authorization: Bearer sk_test_...'.")
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

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	// Stripe also accepts the key as HTTP basic auth username, which many
	// client libraries default to. Supporting both avoids surprising merchants.
	if user, _, ok := r.BasicAuth(); ok {
		return user
	}
	return ""
}

// keyPrefix returns the loggable portion of a key, e.g. "sk_test_a1b2...".
func keyPrefix(raw string) string {
	if len(raw) > 16 {
		return raw[:16] + "..."
	}
	return "malformed"
}
