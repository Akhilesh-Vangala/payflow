// Package ratelimit caps how often a caller may hit an endpoint.
//
// This exists because of a specific hole rather than as general good practice.
// The hosted payment page is public and unauthenticated by necessity — a
// customer paying a link has no account — and so is the vault's tokenize
// endpoint, because the card iframe calls it straight from a browser. Together,
// with no cap, they form a CARD TESTING ORACLE:
//
//	tokenize(stolen_pan) -> start session -> confirm -> "was that card live?"
//
// Repeat ten thousand times. Note what that attack never does: open the iframe.
// So Behavior.Present is false, every behavioral rule short-circuits on its
// guard, and the keystroke-timing work is bypassed entirely. Rate limiting is
// the only control that still bites, which makes it load-bearing rather than
// hygiene.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// Rule is one budget: how many requests, over how long.
type Rule struct {
	// Name appears in logs and metrics. Required — an unnamed limit that starts
	// rejecting traffic is untraceable.
	Name   string
	Limit  int
	Window time.Duration

	// FailOpen decides what happens when Redis is unreachable, and it is a
	// genuine judgement call rather than a default to copy.
	//
	// Fail OPEN on the payment path: a cache outage must not stop every
	// merchant from taking money. The cost is that the card-testing oracle
	// reopens for the duration, so the fallback is logged at Error and is meant
	// to page someone.
	//
	// Fail CLOSED on authentication: if Redis is down, an attacker who can
	// cause that outage would otherwise get unlimited password guesses. Logins
	// are low-volume and briefly refusing them is recoverable; handing out
	// unlimited attempts at a password is not.
	FailOpen bool
}

// Limiter is a fixed-window counter over Redis.
//
// Fixed window, not a sliding log or a token bucket, and the reason is the
// tradeoff rather than laziness. A sliding log stores one entry per request and
// costs memory proportional to traffic — precisely the wrong shape when the
// traffic is an attack. A fixed window is two operations and O(1) memory.
//
// What that buys the attacker: up to 2x the limit across a window boundary
// (all of window N at its end, all of N+1 at its start). For a 20/minute cap
// that means a 40-request burst in the worst case, which does not meaningfully
// help someone testing ten thousand cards. Paying for exactness here would be
// paying to solve a problem the attacker does not have.
type Limiter struct {
	rdb *redis.Client
}

func New(rdb *redis.Client) *Limiter { return &Limiter{rdb: rdb} }

// Result describes one decision.
type Result struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
	// Degraded reports that Redis could not be consulted and the rule's
	// FailOpen setting decided the outcome. Surfaced so callers can log it —
	// silently degrading a security control is how it stays broken.
	Degraded bool
}

// Allow consumes one unit of the caller's budget.
//
// identity is whatever the budget is scoped to — an IP, a merchant id, a link
// code. It is used verbatim in the key, so the caller is responsible for it
// being a bounded value: putting raw user input in here would let an attacker
// fill Redis with distinct keys.
func (l *Limiter) Allow(ctx context.Context, rule Rule, identity string) Result {
	if l == nil || l.rdb == nil {
		return degraded(rule)
	}

	// Bucketing the window into the key is what makes this atomic without a
	// script: the key IS the window, so INCR cannot straddle two of them.
	bucket := time.Now().UTC().UnixNano() / int64(rule.Window)
	key := fmt.Sprintf("rl:%s:%s:%d", rule.Name, identity, bucket)

	pipe := l.rdb.Pipeline()
	incr := pipe.Incr(ctx, key)
	// EXPIRE NX, never a bare EXPIRE. This codebase has already paid for that
	// distinction once: an unconditional EXPIRE on the device-card velocity key
	// slid its window forward on every use, so a device used daily never
	// expired and accumulated cards without bound. Here the same bug would be
	// worse — it would extend the window on every request, so a caller sending
	// continuous traffic would be locked out permanently after their first
	// burst.
	pipe.ExpireNX(ctx, key, rule.Window)

	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		slog.Error("ratelimit: redis unavailable",
			"error", err, "rule", rule.Name, "fail_open", rule.FailOpen)
		return degraded(rule)
	}

	count := int(incr.Val())
	if count > rule.Limit {
		// Reported from the bucket boundary rather than as the full window, so a
		// caller told to wait 30s is actually able to proceed in 30s.
		elapsed := time.Duration(time.Now().UTC().UnixNano() - bucket*int64(rule.Window))
		return Result{Allowed: false, RetryAfter: rule.Window - elapsed}
	}
	return Result{Allowed: true, Remaining: rule.Limit - count}
}

func degraded(rule Rule) Result {
	return Result{
		Allowed:    rule.FailOpen,
		Degraded:   true,
		RetryAfter: rule.Window,
	}
}
