// Command migrate applies database migrations.
//
// This exists because there was no way to change the schema of a database that
// already has data in it. Migrations were only ever applied by Postgres's
// docker-entrypoint-initdb.d, which runs exactly once, on the first boot of an
// EMPTY volume. That is fine on a laptop and useless in production: the second
// deploy has nowhere to put its migration.
//
// Design notes, in the order they matter:
//
//   - Migrations are EMBEDDED in the binary. The image that runs the migration
//     therefore contains the exact SQL it will run, so "which version is this
//     database on, and what did that migration say" is answerable from an image
//     tag months later.
//
//   - Each migration runs inside a transaction WITH its version insert. A
//     migration that half-applied and still counted as done is unrecoverable
//     without a human reading SQL at 3am. Postgres has transactional DDL; this
//     uses it.
//
//   - A session-level advisory lock serializes runners. Rolling deploys start
//     several pods at once, and without this two of them would run 0013
//     simultaneously.
//
// Usage:
//
//	migrate                 apply everything outstanding
//	migrate -status         show what is applied and what is pending
//	migrate -baseline       record every migration as applied WITHOUT running it
//
// -baseline is for adopting a database that was built the old way, by
// initdb. It is the one destructive-by-omission flag here: it tells the tool to
// trust that the schema already matches, which it cannot verify.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/Akhilesh-Vangala/payflow/migrations"
)

// advisoryLockID is an arbitrary constant; it only has to be stable and not
// collide with another advisory lock in this database.
const advisoryLockID = 0x7061796c6f // "payflow"

func main() {
	if err := run(); err != nil {
		slog.Error("migrate: fatal", "error", err)
		os.Exit(1)
	}
}

type migration struct {
	Version string // "0001_init"
	SQL     string
	Hash    string
}

func run() error {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	var (
		status   = flag.Bool("status", false, "print applied and pending migrations, change nothing")
		baseline = flag.Bool("baseline", false, "record all migrations as applied without running them")
	)
	flag.Parse()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("migrate: DATABASE_URL is required")
	}

	all, err := load()
	if err != nil {
		return err
	}

	// A single connection, not a pool: the advisory lock below is session
	// scoped, so it has to be taken and released on the same connection.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("migrate: connect: %w", err)
	}
	defer conn.Close(context.Background())

	if err := ensureTable(ctx, conn); err != nil {
		return err
	}

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return err
	}

	if *status {
		return printStatus(all, applied)
	}

	// Taken before reading the pending list is computed into action, so two
	// runners cannot both decide 0013 is pending.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(advisoryLockID)); err != nil {
		return fmt.Errorf("migrate: acquire advisory lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, int64(advisoryLockID))
	}()

	// Re-read after locking: another runner may have applied migrations while
	// this process was waiting for the lock.
	if applied, err = appliedVersions(ctx, conn); err != nil {
		return err
	}

	pending := 0
	for _, m := range all {
		if prev, ok := applied[m.Version]; ok {
			// A changed hash means someone edited a migration that has already
			// run somewhere. That file will never be re-executed, so the
			// databases have silently diverged — refuse rather than continue.
			if prev != "" && prev != m.Hash {
				return fmt.Errorf(
					"migrate: %s has been modified since it was applied "+
						"(recorded %s, file %s). Applied migrations are immutable — "+
						"write a new one instead of editing this",
					m.Version, prev[:12], m.Hash[:12])
			}
			continue
		}
		pending++

		if *baseline {
			if err := record(ctx, conn, m, 0); err != nil {
				return err
			}
			slog.Info("migrate: baselined", "version", m.Version)
			continue
		}

		if err := apply(ctx, conn, m); err != nil {
			return err
		}
	}

	if pending == 0 {
		slog.Info("migrate: database is up to date", "applied", len(applied))
	}
	return nil
}

// apply runs one migration and records it in the SAME transaction.
//
// This is the property that makes the tool safe to re-run after a crash: there
// is no window in which the SQL has taken effect but the version row has not,
// or the reverse.
func apply(ctx context.Context, conn *pgx.Conn, m migration) error {
	started := time.Now()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate: begin %s: %w", m.Version, err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return fmt.Errorf("migrate: apply %s: %w", m.Version, err)
	}

	took := time.Since(started)
	if _, err := tx.Exec(ctx, `
		INSERT INTO schema_migrations (version, hash, execution_ms)
		VALUES ($1, $2, $3)`, m.Version, m.Hash, took.Milliseconds()); err != nil {
		return fmt.Errorf("migrate: record %s: %w", m.Version, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migrate: commit %s: %w", m.Version, err)
	}

	slog.Info("migrate: applied", "version", m.Version, "took_ms", took.Milliseconds())
	return nil
}

func record(ctx context.Context, conn *pgx.Conn, m migration, ms int64) error {
	_, err := conn.Exec(ctx, `
		INSERT INTO schema_migrations (version, hash, execution_ms)
		VALUES ($1, $2, $3) ON CONFLICT (version) DO NOTHING`, m.Version, m.Hash, ms)
	if err != nil {
		return fmt.Errorf("migrate: record %s: %w", m.Version, err)
	}
	return nil
}

func ensureTable(ctx context.Context, conn *pgx.Conn) error {
	_, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version      TEXT PRIMARY KEY,
			-- SHA-256 of the file as applied. Lets a later run detect that an
			-- already-applied migration was edited, which is how two databases
			-- silently end up with different schemas.
			hash         TEXT NOT NULL,
			execution_ms BIGINT NOT NULL DEFAULT 0,
			applied_at   TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("migrate: create schema_migrations: %w", err)
	}
	return nil
}

func appliedVersions(ctx context.Context, conn *pgx.Conn) (map[string]string, error) {
	rows, err := conn.Query(ctx, `SELECT version, hash FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("migrate: read applied versions: %w", err)
	}
	defer rows.Close()

	applied := map[string]string{}
	for rows.Next() {
		var version, hash string
		if err := rows.Scan(&version, &hash); err != nil {
			return nil, fmt.Errorf("migrate: scan applied version: %w", err)
		}
		applied[version] = hash
	}
	return applied, rows.Err()
}

// load reads the embedded .up.sql files in filename order.
//
// Ordering is lexicographic on the zero-padded prefix, which is why the files
// are named 0001_, 0002_ and not 1_, 2_ — "10" sorts before "2".
func load() ([]migration, error) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("migrate: read embedded migrations: %w", err)
	}

	var out []migration
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", name, err)
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{
			Version: strings.TrimSuffix(name, ".up.sql"),
			SQL:     string(body),
			Hash:    hex.EncodeToString(sum[:]),
		})
	}
	if len(out) == 0 {
		return nil, errors.New("migrate: no migrations were embedded in this binary")
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func printStatus(all []migration, applied map[string]string) error {
	fmt.Printf("\n  %-34s %s\n", "MIGRATION", "STATE")
	fmt.Printf("  %s\n", strings.Repeat("-", 48))
	pending := 0
	for _, m := range all {
		state := "pending"
		if _, ok := applied[m.Version]; ok {
			state = "applied"
		} else {
			pending++
		}
		fmt.Printf("  %-34s %s\n", m.Version, state)
	}
	fmt.Printf("\n  %d applied, %d pending\n\n", len(applied), pending)
	return nil
}
