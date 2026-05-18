// Command scheduler runs the periodic jobs (§9).
//
// Reconciliation, payouts, and retention. Dunning retries belong here when
// they are built.
//
// Idempotency-key cleanup deliberately does NOT live here — deleting a key
// changes money behaviour, because a retry carrying a pruned key creates a
// second charge instead of replaying the first. See internal/retention.
//
// Jobs are run in-process on a ticker rather than via cron, so a job that
// overruns its interval delays the next tick instead of overlapping with
// itself. Reconciliation is idempotent and would survive an overlap, but
// relying on that is worse than not creating the situation.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Akhilesh-Vangala/payflow/internal/config"
	"github.com/Akhilesh-Vangala/payflow/internal/db"
	"github.com/Akhilesh-Vangala/payflow/internal/payments"
	"github.com/Akhilesh-Vangala/payflow/internal/payouts"
	"github.com/Akhilesh-Vangala/payflow/internal/reconcile"
	"github.com/Akhilesh-Vangala/payflow/internal/retention"
)

func main() {
	if err := run(); err != nil {
		slog.Error("scheduler: fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	bank := payments.NewBankClient(cfg.BankSimulatorURL, cfg.BankTimeout)
	reconciler := reconcile.NewService(pool, bank)

	payoutSvc := payouts.NewService(pool, bank)

	retentionSvc := retention.NewService(pool, retention.DefaultPolicy())

	reconcileEvery := envDuration("RECONCILE_INTERVAL", time.Hour)
	payoutEvery := envDuration("PAYOUT_INTERVAL", 24*time.Hour)
	// Hourly rather than daily: each pass deletes a bounded batch per table, so
	// running often keeps every pass small. A daily job on a busy system would
	// have to delete a day's backlog in one go.
	retentionEvery := envDuration("RETENTION_INTERVAL", time.Hour)

	reconcileTick := time.NewTicker(reconcileEvery)
	defer reconcileTick.Stop()
	payoutTick := time.NewTicker(payoutEvery)
	defer payoutTick.Stop()
	retentionTick := time.NewTicker(retentionEvery)
	defer retentionTick.Stop()

	slog.Info("scheduler started",
		"reconcile_interval", reconcileEvery.String(),
		"payout_interval", payoutEvery.String(),
		"retention_interval", retentionEvery.String())

	// Run once at startup rather than waiting a full interval — a restart
	// after an incident is exactly when there is most likely to be something
	// parked and waiting.
	runReconciliation(ctx, reconciler)

	for {
		select {
		case <-ctx.Done():
			slog.Info("scheduler shutting down")
			return nil
		case <-reconcileTick.C:
			runReconciliation(ctx, reconciler)
		case <-payoutTick.C:
			runPayouts(ctx, payoutSvc)
		case <-retentionTick.C:
			jobCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			if _, err := retentionSvc.Run(jobCtx); err != nil {
				slog.Error("retention: run failed", "error", err)
			}
			cancel()
		}
	}
}

func runPayouts(ctx context.Context, p *payouts.Service) {
	jobCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	result, err := p.Run(jobCtx)
	if err != nil {
		slog.Error("scheduler: payout run failed", "error", err)
		return
	}
	slog.Info("scheduler: payouts complete",
		"merchants_considered", result.MerchantsConsidered,
		"payouts_created", result.PayoutsCreated,
		"total_cents", result.TotalCents,
		"skipped", result.Skipped)
}

func runReconciliation(ctx context.Context, r *reconcile.Service) {
	// Bounded so a hung processor cannot stall the scheduler indefinitely.
	jobCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	result, err := r.Run(jobCtx)
	if err != nil {
		slog.Error("scheduler: reconciliation failed", "error", err)
		return
	}

	slog.Info("scheduler: reconciliation complete",
		"run_id", result.RunID,
		"charges_checked", result.ChargesChecked,
		"charges_resolved", result.ChargesResolved,
		"discrepancies", result.Discrepancies)
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
