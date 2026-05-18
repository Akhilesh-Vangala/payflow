// Package ledger implements append-only double-entry bookkeeping (§5).
//
// Two rules hold everywhere in this package:
//
//  1. Nothing is ever mutated. Corrections are new, balanced entries that
//     reference the original transaction — reversals, not edits.
//  2. Every transaction balances: debits equal credits, per currency.
//
// Rule 2 is enforced three times over — in Go before the write, by a deferred
// Postgres constraint trigger at COMMIT, and again by the reconciliation job.
// Belt and braces is proportionate when the failure mode is unaccounted money.
package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/Akhilesh-Vangala/payflow/internal/money"
)

// Account types. Merchant-scoped accounts carry a merchant_id; platform
// accounts are singletons per currency.
const (
	AccountMerchantBalance    = "merchant_balance"
	AccountCustomerReceivable = "customer_receivable"
	AccountPlatformFees       = "platform_fees"
	AccountInTransit          = "in_transit"
	AccountPaidOut            = "paid_out"
	AccountMerchantDebt       = "merchant_debt"
	AccountReserve            = "reserve"
)

// Entry types, recording what caused a pair of entries to exist.
const (
	EntryCharge          = "charge"
	EntryRefund          = "refund"
	EntryDispute         = "dispute"
	EntryDisputeReversal = "dispute_reversal"
	EntryPayout          = "payout"
	EntryFee             = "fee"
)

const (
	DirectionDebit  = "debit"
	DirectionCredit = "credit"
)

var (
	ErrUnbalanced      = errors.New("ledger: transaction does not balance")
	ErrNoLegs          = errors.New("ledger: transaction has no entries")
	ErrAccountNotFound = errors.New("ledger: account not found")
)

// Leg is one side of a double-entry transaction.
type Leg struct {
	AccountID uuid.UUID
	Direction string
	Amount    money.Amount
	EntryType string
	Metadata  map[string]any
}

func Debit(accountID uuid.UUID, amount money.Amount, entryType string) Leg {
	return Leg{AccountID: accountID, Direction: DirectionDebit, Amount: amount, EntryType: entryType}
}

func Credit(accountID uuid.UUID, amount money.Amount, entryType string) Leg {
	return Leg{AccountID: accountID, Direction: DirectionCredit, Amount: amount, EntryType: entryType}
}

// Transaction is a set of legs written atomically.
type Transaction struct {
	ID             uuid.UUID
	Legs           []Leg
	IdempotencyKey string
}

// Validate checks the balance invariant before touching the database.
//
// The DB trigger would catch an imbalance anyway, but failing here gives a
// clear Go-level error at the call site instead of an opaque constraint
// violation surfacing at COMMIT, potentially several function calls away.
func (t Transaction) Validate() error {
	if len(t.Legs) == 0 {
		return ErrNoLegs
	}

	// Per-currency, because a balanced pair may never mix currencies (§20).
	delta := make(map[string]int64)
	for i, leg := range t.Legs {
		if err := leg.Amount.Validate(); err != nil {
			return fmt.Errorf("leg %d: %w", i, err)
		}
		switch leg.Direction {
		case DirectionDebit:
			delta[leg.Amount.Currency] += leg.Amount.Cents
		case DirectionCredit:
			delta[leg.Amount.Currency] -= leg.Amount.Cents
		default:
			return fmt.Errorf("leg %d: invalid direction %q", i, leg.Direction)
		}
	}

	for currency, d := range delta {
		if d != 0 {
			return fmt.Errorf("%w: %s is off by %d minor units", ErrUnbalanced, currency, d)
		}
	}
	return nil
}

// Post writes a balanced transaction and updates cached balances.
//
// It takes a pgx.Tx rather than a pool because the caller must be able to
// commit the ledger write, the idempotency record, and the outbox row in ONE
// transaction (§4.2 step 5, §22.1). Owning its own transaction here would
// reintroduce exactly the inconsistency the outbox pattern exists to prevent.
func Post(ctx context.Context, tx pgx.Tx, t Transaction) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}

	// All legs in one multi-row INSERT rather than one statement each.
	//
	// A charge posts 2-3 legs and a dispute 4, each previously a separate round
	// trip inside the caller's transaction. Batching does not change what is
	// written, the order it is written in, or when it is validated — the
	// deferred constraint trigger still fires per row at COMMIT and still checks
	// the transaction as a unit.
	//
	// unnest() rather than generated VALUES tuples: the statement text is then
	// identical whatever the leg count, so 2-leg charges, 3-leg charges and
	// 4-leg disputes share ONE prepared statement instead of occupying three
	// separate entries in every connection's statement cache.
	n := len(t.Legs)
	accountIDs := make([]uuid.UUID, n)
	directions := make([]string, n)
	amounts := make([]int64, n)
	currencies := make([]string, n)
	entryTypes := make([]string, n)
	metadatas := make([][]byte, n)

	for i, leg := range t.Legs {
		metadata := leg.Metadata
		if metadata == nil {
			metadata = map[string]any{}
		}
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("ledger: encode metadata for account %s: %w", leg.AccountID, err)
		}
		accountIDs[i] = leg.AccountID
		directions[i] = leg.Direction
		amounts[i] = leg.Amount.Cents
		currencies[i] = leg.Amount.Currency
		entryTypes[i] = leg.EntryType
		metadatas[i] = encoded
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries
			(transaction_id, account_id, direction, amount_cents,
			 currency, entry_type, idempotency_key, metadata)
		SELECT $1, a.account_id, a.direction, a.amount_cents,
		       a.currency, a.entry_type, $7, a.metadata::jsonb
		FROM unnest($2::uuid[], $3::text[], $4::bigint[],
		            $5::text[], $6::text[], $8::text[])
		     AS a(account_id, direction, amount_cents, currency, entry_type, metadata)`,
		t.ID, accountIDs, directions, amounts, currencies, entryTypes,
		nullString(t.IdempotencyKey), metadatas,
	); err != nil {
		return fmt.Errorf("ledger: insert entries for transaction %s: %w", t.ID, err)
	}
	// No balance write here, deliberately.
	//
	// This loop used to follow each entry with an upsert on ledger_balances,
	// taking an exclusive row lock held until the caller's transaction
	// committed. Balances are now checkpoints advanced by the scheduler and
	// summed forward at read time — exact, and off the write path entirely.
	// See Balance, AdvanceCheckpoints, and migration 0013.
	return nil
}

// Balance returns an account's balance: the checkpoint plus everything posted
// since it.
//
// This is EXACT, not eventually consistent, and the distinction is the whole
// point. The checkpoint is a durable partial sum; the tail is summed at read
// time from ledger_entries, which is the source of truth. Advancing the
// checkpoint changes how much tail there is to sum, never the answer.
//
// It replaces an incrementally-maintained cache that the write path updated
// under an exclusive row lock per leg. That lock — three per charge, one of them
// on a row shared by every merchant on the platform — was the system's
// throughput ceiling: upserts observed blocking 3.3-6.5 seconds, and throughput
// pinned at 75 TPS regardless of merchant count. See migration 0013.
func Balance(ctx context.Context, q Querier, accountID uuid.UUID) (money.Amount, error) {
	var cents int64
	var currency string
	err := q.QueryRow(ctx, `
		SELECT b.balance_cents + COALESCE((
		           SELECT SUM(CASE WHEN e.direction = 'debit' THEN e.amount_cents
		                           ELSE -e.amount_cents END)
		           FROM ledger_entries e
		           WHERE e.account_id = b.account_id
		             AND e.id > b.through_entry_id
		       ), 0),
		       b.currency
		FROM ledger_balances b
		WHERE b.account_id = $1`,
		accountID,
	).Scan(&cents, &currency)
	if errors.Is(err, pgx.ErrNoRows) {
		return money.Amount{}, ErrAccountNotFound
	}
	if err != nil {
		return money.Amount{}, fmt.Errorf("ledger: read balance: %w", err)
	}
	return money.Amount{Cents: cents, Currency: currency}, nil
}

// AdvanceCheckpoints folds posted entries into the stored balances.
//
// Purely an optimisation: it shortens the tail that Balance has to sum and
// changes no answer. Safe to run at any time, safe to skip, safe to run
// concurrently with charges — it reads a snapshot of max(id) first, so entries
// written while it runs simply land after the new checkpoint.
//
// Called by the scheduler. Nothing on the payment path calls it, which is the
// entire reason the payment path no longer takes a balance lock.
func AdvanceCheckpoints(ctx context.Context, q Querier) (int64, error) {
	var updated int64
	err := q.QueryRow(ctx, `
		WITH snapshot AS (
		    SELECT COALESCE(max(id), 0) AS max_id FROM ledger_entries
		),
		deltas AS (
		    SELECT b.account_id,
		           COALESCE(SUM(CASE WHEN e.direction = 'debit' THEN e.amount_cents
		                             ELSE -e.amount_cents END), 0) AS delta
		    FROM ledger_balances b
		    CROSS JOIN snapshot s
		    LEFT JOIN ledger_entries e
		           ON e.account_id = b.account_id
		          AND e.id > b.through_entry_id
		          AND e.id <= s.max_id
		    WHERE b.through_entry_id < s.max_id
		    GROUP BY b.account_id
		),
		applied AS (
		    UPDATE ledger_balances b
		    SET balance_cents    = b.balance_cents + d.delta,
		        through_entry_id = (SELECT max_id FROM snapshot),
		        version          = b.version + 1,
		        updated_at       = now()
		    FROM deltas d
		    WHERE d.account_id = b.account_id
		    RETURNING 1
		)
		SELECT count(*) FROM applied`).Scan(&updated)
	if err != nil {
		return 0, fmt.Errorf("ledger: advance checkpoints: %w", err)
	}
	return updated, nil
}

// DerivedBalance recomputes an account's balance from ledger_entries, ignoring
// the cache. This is the authoritative figure; Balance is merely fast. The
// reconciliation job compares the two and alerts on any divergence (§24.3).
func DerivedBalance(ctx context.Context, q Querier, accountID uuid.UUID) (money.Amount, error) {
	var cents int64
	var currency string
	err := q.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE WHEN direction = 'debit' THEN amount_cents
		                         ELSE -amount_cents END), 0),
		       COALESCE(MIN(currency), '')
		FROM ledger_entries WHERE account_id = $1`,
		accountID,
	).Scan(&cents, &currency)
	if err != nil {
		return money.Amount{}, fmt.Errorf("ledger: derive balance: %w", err)
	}
	return money.Amount{Cents: cents, Currency: currency}, nil
}

// accountCache memoises (database, merchant, type, currency) -> account id.
//
// Safe to cache forever, and that is a property of the data rather than a bet:
// an account row is immutable once created. Its id never changes, it is never
// deleted while entries reference it, and UNIQUE (merchant_id, account_type,
// currency) means one identity maps to exactly one id for all time. So there is
// nothing to invalidate.
//
// The key includes the DATABASE, not just the account identity. Without that,
// the integration tests would poison it: testcontainers gives each test its own
// Postgres in the SAME process, so a cached id from one test's database would be
// returned for another's and fail a foreign key. Including host/port/database
// makes each container a distinct namespace, and in production there is one
// entry per real account.
//
// Worth 3 SELECTs per charge — EnsureAccount was called once per ledger leg and
// cached nowhere.
var accountCache sync.Map // string -> uuid.UUID

func accountCacheKey(tx pgx.Tx, merchantID uuid.UUID, accountType, currency string) string {
	cfg := tx.Conn().Config()
	return fmt.Sprintf("%s:%d/%s|%s|%s|%s",
		cfg.Host, cfg.Port, cfg.Database, merchantID, accountType, currency)
}

// EnsureAccount looks up an account, creating it if absent, and returns its ID.
// merchantID may be uuid.Nil for platform-level accounts.
func EnsureAccount(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, accountType, currency string) (uuid.UUID, error) {
	cacheKey := accountCacheKey(tx, merchantID, accountType, currency)
	if cached, ok := accountCache.Load(cacheKey); ok {
		return cached.(uuid.UUID), nil
	}

	var id uuid.UUID
	var merchantArg any
	query := `SELECT id FROM accounts WHERE account_type = $1 AND currency = $2 AND merchant_id `

	if merchantID == uuid.Nil {
		merchantArg = nil
		query += `IS NULL`
	} else {
		merchantArg = merchantID
		query += `= $3`
	}

	var err error
	if merchantID == uuid.Nil {
		err = tx.QueryRow(ctx, query, accountType, currency).Scan(&id)
	} else {
		err = tx.QueryRow(ctx, query, accountType, currency, merchantArg).Scan(&id)
	}
	if err == nil {
		accountCache.Store(cacheKey, id)
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("ledger: lookup account: %w", err)
	}

	// Race-safe create: a concurrent caller may insert the same account between
	// our SELECT and INSERT, so fall back to re-reading on conflict.
	err = tx.QueryRow(ctx, `
		INSERT INTO accounts (merchant_id, account_type, currency)
		VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		merchantArg, accountType, currency,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		if merchantID == uuid.Nil {
			err = tx.QueryRow(ctx, query, accountType, currency).Scan(&id)
		} else {
			err = tx.QueryRow(ctx, query, accountType, currency, merchantArg).Scan(&id)
		}
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("ledger: create account: %w", err)
	}

	// Seed the balance checkpoint alongside the account.
	//
	// Since migration 0013 the write path no longer touches ledger_balances at
	// all, so nothing else would ever create this row — Balance would return
	// ErrAccountNotFound for every account created after the migration, and the
	// reconciliation drift check would silently have nothing to compare.
	//
	// Starting at (0, 0) is correct rather than merely convenient: a brand new
	// account has no entries, and through_entry_id = 0 means Balance sums
	// everything from the beginning, which for this account is nothing. The
	// scheduler advances it from there.
	//
	// This costs one INSERT per account LIFETIME, not per charge — which is the
	// whole difference between this and what it replaced.
	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger_balances (account_id, balance_cents, currency, version, updated_at, through_entry_id)
		VALUES ($1, 0, $2, 1, now(), 0)
		ON CONFLICT (account_id) DO NOTHING`,
		id, currency,
	); err != nil {
		return uuid.Nil, fmt.Errorf("ledger: seed balance checkpoint: %w", err)
	}
	return id, nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
