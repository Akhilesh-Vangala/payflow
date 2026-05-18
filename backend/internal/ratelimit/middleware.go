package ratelimit

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Akhilesh-Vangala/payflow/internal/httpx"
)

// Rules used across the services. Kept together so the whole budget is
// reviewable in one place rather than scattered over route definitions.
//
// The numbers are set against what a REAL user does, not against what feels
// generous. A customer pays a link once. A merchant signs in a few times a day.
// Anything an order of magnitude past that is not a user.
var (
	// The card-testing control. Twenty tokenizations per minute per IP is
	// roughly ten times what a fumbling customer produces and roughly a
	// thousandth of what a testing script wants.
	Tokenize = Rule{Name: "tokenize", Limit: 20, Window: time.Minute, FailOpen: true}

	// Confirms are the expensive half — each one reaches the bank. Tighter than
	// tokenize because a legitimate customer confirms once or twice.
	Confirm = Rule{Name: "confirm", Limit: 10, Window: time.Minute, FailOpen: true}

	// Per LINK as well as per IP, because a distributed attack from many
	// addresses still concentrates on one merchant's link. This is the limit
	// that catches a botnet; the per-IP one catches a script.
	ConfirmPerLink = Rule{Name: "confirm_link", Limit: 60, Window: time.Minute, FailOpen: true}

	// Fail CLOSED. See Rule.FailOpen — unlimited password guessing is a worse
	// outcome than briefly refusing logins.
	Login = Rule{Name: "login", Limit: 10, Window: 10 * time.Minute, FailOpen: false}

	// Signup is not a thing a person does repeatedly. Also fails closed: the
	// downside is an inconvenienced new customer, against unbounded account
	// creation.
	Register = Rule{Name: "register", Limit: 5, Window: time.Hour, FailOpen: false}
)

// KeyFunc extracts the identity a budget is scoped to.
type KeyFunc func(*http.Request) string

// ByIP is the default for unauthenticated endpoints.
//
// It is the best available option and a weak one, which is worth stating
// plainly: X-Forwarded-For is caller-controlled, so an attacker behind a proxy
// they control can rotate it freely and defeat this. Behind a load balancer
// that OVERWRITES the header — an AWS ALB does — it is trustworthy. Deployed
// without such a proxy in front, this limit is decorative, and the per-link
// budget is what actually holds.
func ByIP(r *http.Request) string {
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

// Middleware enforces one rule, keyed by the given function.
func Middleware(l *Limiter, rule Rule, key KeyFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity := key(r)
			result := l.Allow(r.Context(), rule, identity)

			if result.Degraded {
				slog.Error("ratelimit: degraded",
					"rule", rule.Name,
					"allowed", result.Allowed,
					"path", r.URL.Path,
					"trace_id", httpx.TraceID(r.Context()))
			}

			if result.Allowed {
				next.ServeHTTP(w, r)
				return
			}

			retry := int(result.RetryAfter.Seconds()) + 1
			w.Header().Set("Retry-After", strconv.Itoa(retry))

			// Logged at Warn with the identity, because this is the signal an
			// attack is in progress. A 429 that leaves no trace tells you
			// nothing after the fact, and "which IP tripped which limit, when"
			// is the entire investigation.
			slog.Warn("ratelimit: blocked",
				"rule", rule.Name,
				"identity", identity,
				"path", r.URL.Path,
				"retry_after_s", retry,
				"user_agent", r.UserAgent(),
				"trace_id", httpx.TraceID(r.Context()))

			httpx.Fail(w, http.StatusTooManyRequests, httpx.TypeRateLimit,
				"rate_limit_exceeded",
				"Too many requests. Please wait a moment and try again.")
		})
	}
}
