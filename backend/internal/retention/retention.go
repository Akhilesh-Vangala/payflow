// Package retention deletes operational data that has outlived its purpose.
//
// Before this, the only scheduled deletion in the entire system was the vault's
// token purge. Everything else grew forever: outbox events that had already been
// published, delivery attempt logs, expired browser sessions, abandoned checkout
// sessions, access logs, completed reconciliation runs.
//
// WHAT IS DELIBERATELY NOT PRUNED HERE
//
//	ledger_entries      the source of truth for money. Append-only, never
//	                    deleted. Growth is handled by partitioning, not deletion.
//	charges, refunds,
//	disputes, payouts   the financial record. Same reasoning.
//	idempotency_keys    deleting one CHANGES MONEY BEHAVIOUR — a retry carrying a
//	                    pruned key would create a second charge rather than
//	                    replaying the first. The table has an expires_at column
//	                    and an index built for exactly this, but nothing in
//	                    idempotency.Begin ever reads them, so keys are currently
//	                    honoured forever. Making that TTL real is a deliberate
//	                    product decision about how long a retry stays safe, and
//	                    it belongs in the idempotency layer where it can be
//	                    enforced explicitly — not as a side effect of a cleanup
//	                    job. Left alone on purpose.
//	reconciliation_
//	discrepancies       unresolved ones are open findings about unaccounted
//	                    money. Only resolved rows age out.
package retention

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Policy is how long each kind of operational record is kept.
type Policy struct {
	PublishedEvents   time.Duration
	WebhookDeliveries time.Duration
	ExpiredSessions   time.Duration
	CheckoutSessions  time.Duration
	VaultAccessLog    time.Duration
	ReconcileRuns     time.Duration
}

// DefaultPolicy keeps enough history to investigate an incident, and no more.
//
// Webhook deliveries outlive their events because "did you send it?" is the
// question a merchant asks weeks later. The vault access log is kept longest:
// it is the audit trail for who touched card data, which is the one question
// worth being able to answer late.
func DefaultPolicy() Policy {
	return Policy{
		PublishedEvents:   7 * 24 * time.Hour,
		WebhookDeliveries: 30 * 24 * time.Hour,
		ExpiredSessions:   7 * 24 * time.Hour,
		CheckoutSessions:  30 * 24 * time.Hour,
		VaultAccessLog:    90 * 24 * time.Hour,
		ReconcileRuns:     90 * 24 * time.Hour,
	}
}

type Service struct {
	pool   *pgxpool.Pool
	policy Policy
	// batch caps rows deleted per statement. Deleting millions in one statement
	// holds locks and bloats WAL; several bounded passes are kinder to a live
	// system than one large one.
	batch int
}

func NewService(pool *pgxpool.Pool, policy Policy) *Service {
	return &Service{pool: pool, policy: policy, batch: 10_000}
}

type Result struct {
	OutboxEvents      int64
	WebhookDeliveries int64
	WebhookAttempts   int64
	MerchantSessions  int64
	CheckoutSessions  int64
	VaultAccessLog    int64
	ReconcileRuns     int64
}

// Run deletes one batch from each table.
//
// Every statement is bounded by both a predicate and a LIMIT, and each runs in
// its own transaction. A failure on one table therefore does not roll back the
// others or leave a long transaction open across all of them.
func (s *Service) Run(ctx context.Context) (*Result, error) {
	r := &Result{}
	now := time.Now()

	// Published outbox events. Safe because publication is what the row exists
	// to record, and the delivery it created is tracked separately in
	// webhook_deliveries with its own retention.
	r.OutboxEvents = s.prune(ctx, "outbox_events", `
		DELETE FROM outbox_events WHERE id IN (
			SELECT id FROM outbox_events
			WHERE published AND published_at < $1
			ORDER BY id LIMIT $2)`,
		now.Add(-s.policy.PublishedEvents))

	// Attempts before deliveries, so the child rows go first and the parent
	// delete never has to cascade a large set.
	r.WebhookAttempts = s.prune(ctx, "webhook_delivery_attempts", `
		DELETE FROM webhook_delivery_attempts WHERE id IN (
			SELECT a.id FROM webhook_delivery_attempts a
			JOIN webhook_deliveries d ON d.id = a.delivery_id
			WHERE d.status IN ('delivered', 'dead') AND d.created_at < $1
			ORDER BY a.id LIMIT $2)`,
		now.Add(-s.policy.WebhookDeliveries))

	// Only terminal deliveries. A 'pending' row is still being retried, and a
	// 'failed' one may yet be picked up — deleting either would silently drop a
	// notification a merchant is waiting on.
	r.WebhookDeliveries = s.prune(ctx, "webhook_deliveries", `
		DELETE FROM webhook_deliveries WHERE id IN (
			SELECT id FROM webhook_deliveries
			WHERE status IN ('delivered', 'dead') AND created_at < $1
			ORDER BY created_at LIMIT $2)`,
		now.Add(-s.policy.WebhookDeliveries))

	r.MerchantSessions = s.prune(ctx, "merchant_sessions", `
		DELETE FROM merchant_sessions WHERE id IN (
			SELECT id FROM merchant_sessions
			WHERE (expires_at < $1 OR revoked_at IS NOT NULL)
			ORDER BY created_at LIMIT $2)`,
		now.Add(-s.policy.ExpiredSessions))

	// Terminal checkout sessions only. An 'open' one may still be on a
	// customer's screen, and a 'processing' one is an unresolved payment that
	// reconciliation has not finished with.
	r.CheckoutSessions = s.prune(ctx, "checkout_sessions", `
		DELETE FROM checkout_sessions WHERE id IN (
			SELECT id FROM checkout_sessions
			WHERE status IN ('completed', 'failed', 'expired') AND created_at < $1
			ORDER BY created_at LIMIT $2)`,
		now.Add(-s.policy.CheckoutSessions))

	r.VaultAccessLog = s.prune(ctx, "vault_access_log", `
		DELETE FROM vault_access_log WHERE id IN (
			SELECT id FROM vault_access_log
			WHERE created_at < $1 ORDER BY id LIMIT $2)`,
		now.Add(-s.policy.VaultAccessLog))

	// Runs with no unresolved discrepancies attached. An open discrepancy means
	// money is unaccounted for, and its run is the context for investigating it.
	r.ReconcileRuns = s.prune(ctx, "reconciliation_runs", `
		DELETE FROM reconciliation_runs WHERE id IN (
			SELECT r.id FROM reconciliation_runs r
			WHERE r.finished_at < $1
			  AND NOT EXISTS (SELECT 1 FROM reconciliation_discrepancies d
			                  WHERE d.run_id = r.id AND d.resolved_at IS NULL)
			ORDER BY r.started_at LIMIT $2)`,
		now.Add(-s.policy.ReconcileRuns))

	total := r.OutboxEvents + r.WebhookDeliveries + r.WebhookAttempts +
		r.MerchantSessions + r.CheckoutSessions + r.VaultAccessLog + r.ReconcileRuns
	if total > 0 {
		slog.Info("retention: pruned",
			"total", total,
			"outbox_events", r.OutboxEvents,
			"webhook_deliveries", r.WebhookDeliveries,
			"webhook_attempts", r.WebhookAttempts,
			"merchant_sessions", r.MerchantSessions,
			"checkout_sessions", r.CheckoutSessions,
			"vault_access_log", r.VaultAccessLog,
			"reconciliation_runs", r.ReconcileRuns)
	}
	return r, nil
}

// prune runs one bounded delete. Errors are logged, never returned: retention is
// housekeeping, and a failure to reclaim space must not take down the scheduler
// that also runs reconciliation.
func (s *Service) prune(ctx context.Context, table, query string, before time.Time) int64 {
	tag, err := s.pool.Exec(ctx, query, before, s.batch)
	if err != nil {
		slog.Error("retention: prune failed", "table", table, "error",
			fmt.Errorf("retention: prune %s: %w", table, err))
		return 0
	}
	return tag.RowsAffected()
}
