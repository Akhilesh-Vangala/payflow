// Command payments-api serves the public payments API (§3).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof on the default mux, served on PPROF_ADDR only
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/Akhilesh-Vangala/payflow/internal/accounts"
	"github.com/Akhilesh-Vangala/payflow/internal/checkout"
	"github.com/Akhilesh-Vangala/payflow/internal/config"
	"github.com/Akhilesh-Vangala/payflow/internal/dashboard"
	"github.com/Akhilesh-Vangala/payflow/internal/db"
	"github.com/Akhilesh-Vangala/payflow/internal/httpx"
	"github.com/Akhilesh-Vangala/payflow/internal/payments"
	"github.com/Akhilesh-Vangala/payflow/internal/ratelimit"
	"github.com/Akhilesh-Vangala/payflow/internal/risk"
	"github.com/Akhilesh-Vangala/payflow/internal/vault"
	"github.com/redis/go-redis/v9"
)

func main() {
	if err := run(); err != nil {
		slog.Error("payments-api: fatal", "error", err)
		os.Exit(1)
	}
}

// dashboardCORS lets the dashboard SPA, served from its own origin, call this
// API. Origins are an explicit allowlist — a wildcard would let any site on
// the internet make authenticated requests on a merchant's behalf.
func dashboardCORS(allowed []string) func(http.Handler) http.Handler {
	set := make(map[string]bool, len(allowed))
	for _, o := range allowed {
		set[o] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if origin := r.Header.Get("Origin"); origin != "" && set[origin] {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
				h.Add("Vary", "Origin")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func splitOrigins(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Configured, not hardcoded. The per-request log line is the single most
	// frequent thing this process does, and at a few thousand requests a second
	// it is a real share of the CPU — LOG_LEVEL=warn has to be able to turn it
	// off without a code change.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.SlogLevel(),
	})))

	// pprof on a loopback-only port, never on the public listener.
	//
	// Exposing /debug/pprof on the API would let anyone dump the heap of a
	// process that handles payment tokens. Bound to 127.0.0.1 it is reachable
	// via `kubectl port-forward` when someone is actually investigating, and
	// from nowhere else.
	if addr := os.Getenv("PPROF_ADDR"); addr != "" {
		go func() {
			slog.Info("pprof listening", "addr", addr)
			if err := http.ListenAndServe(addr, nil); err != nil {
				slog.Warn("pprof listener stopped", "error", err)
			}
		}()
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	bank := payments.NewBankClient(cfg.BankSimulatorURL, cfg.BankTimeout)
	vaultClient := vault.NewPaymentsAdapter(
		vault.NewClient(cfg.VaultURL, cfg.VaultInternalSecret, cfg.VaultTimeout),
	)

	// Redis backs the velocity counters. A failure to connect is logged but not
	// fatal: velocity rules go quiet without it, and the rest of the risk
	// engine still runs. Refusing to start would turn a cache outage into a
	// payments outage, which is precisely the coupling §14.3 warns against.
	var counter *risk.Counter
	var limiter *ratelimit.Limiter
	if opts, err := redis.ParseURL(cfg.RedisURL); err != nil {
		slog.Warn("payments-api: invalid REDIS_URL, velocity rules disabled", "error", err)
	} else {
		rdb := redis.NewClient(opts)
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err := rdb.Ping(pingCtx).Err(); err != nil {
			slog.Warn("payments-api: redis unreachable, velocity rules disabled", "error", err)
		} else {
			counter = risk.NewCounter(rdb)
			// Same client. The rate limiter's keys are namespaced under "rl:"
			// and the velocity counters under "vel:", so they share a
			// connection pool without sharing a keyspace.
			limiter = ratelimit.New(rdb)
			defer rdb.Close()
		}
		cancel()
	}

	assessor := risk.NewService(
		risk.NewEngine(nil),
		counter,
		risk.NewClient(cfg.FraudServiceURL, cfg.FraudTimeout),
	)

	paymentsSvc := payments.NewService(pool, bank, vaultClient, assessor)
	handler := payments.NewHandler(paymentsSvc)
	dashboardHandler := dashboard.NewHandler(dashboard.NewService(pool))
	accountsHandler := accounts.NewHandler(accounts.NewService(pool), limiter)
	// The hosted checkout creates charges on a merchant's behalf, so it holds
	// the payments service directly rather than calling the public API with a
	// key it would have to store somewhere.
	checkoutHandler := checkout.NewHandler(
		checkout.NewService(pool, paymentsSvc), limiter, cfg.VaultURL, cfg.PublicURL)

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(httpx.Trace)
	r.Use(httpx.LogRequests)
	// Above the route table on purpose. chi answers OPTIONS on a GET-only
	// route with 405 before any group middleware runs, so a preflight
	// registered inside the authenticated group would never be reached and
	// every browser request would fail.
	r.Use(dashboardCORS(splitOrigins(cfg.DashboardOrigins)))

	// Unauthenticated: liveness and readiness must answer even when the
	// database is unhealthy, or Kubernetes cannot distinguish "starting up"
	// from "dependency down" and will restart pods that are working fine.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", func(w http.ResponseWriter, req *http.Request) {
		pingCtx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(pingCtx); err != nil {
			httpx.JSON(w, http.StatusServiceUnavailable,
				map[string]string{"status": "degraded", "reason": "database unreachable"})
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	// Unauthenticated by necessity: signing up is how you get a credential, and
	// a customer paying a link has no account here and never will.
	accountsHandler.PublicRoutes(r)
	checkoutHandler.PublicRoutes(r)

	// Either credential: an API key for a merchant's server, or a dashboard
	// session for a person. Both resolve to the same merchant.
	r.Group(func(r chi.Router) {
		r.Use(httpx.Authenticate(pool))
		handler.Routes(r)
		dashboardHandler.Routes(r)
	})

	// A signed-in person only. An API key must not be able to mint another API
	// key, or a leaked read-only key escalates itself in one request.
	r.Group(func(r chi.Router) {
		r.Use(httpx.Authenticate(pool))
		r.Use(httpx.RequireSession)
		accountsHandler.SessionRoutes(r)
		checkoutHandler.SessionRoutes(r)
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Must exceed the bank timeout, or this server would cut off a request
		// that is legitimately waiting on the processor.
		WriteTimeout: cfg.BankTimeout + 25*time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		slog.Info("payments-api listening", "port", cfg.Port, "env", cfg.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("payments-api: listen failed", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	// Drain in-flight requests before exiting. Killing a pod mid-charge is
	// exactly how a charge ends up in an ambiguous state that reconciliation
	// then has to clean up — worth avoiding on a routine deploy.
	slog.Info("payments-api shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
