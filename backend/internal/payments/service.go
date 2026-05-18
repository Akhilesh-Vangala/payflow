package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/Akhilesh-Vangala/payflow/internal/db"
	"github.com/Akhilesh-Vangala/payflow/internal/httpx"
	"github.com/Akhilesh-Vangala/payflow/internal/idempotency"
	"github.com/Akhilesh-Vangala/payflow/internal/ledger"
	"github.com/Akhilesh-Vangala/payflow/internal/money"
	"github.com/Akhilesh-Vangala/payflow/internal/risk"
)

// Charge statuses.
const (
	StatusPending                = "pending"
	StatusRequiresAction         = "requires_action"
	StatusSucceeded              = "succeeded"
	StatusFailed                 = "failed"
	StatusRequiresReconciliation = "requires_reconciliation"
)

// VaultClient is the payments service's view of the vault (§2.4).
//
// Narrowed to an interface here rather than taking *vault.Client directly, so
// this package has no dependency on the vault package and tests can supply a
// stand-in. It is also a standing reminder of the boundary: these two methods
// are the only way card data enters the charge flow.
type VaultClient interface {
	// Metadata returns brand, last4, BIN and fingerprint — never a PAN.
	Metadata(ctx context.Context, token string) (*CardMetadata, error)
	// Detokenize returns a raw PAN for immediate submission to the processor.
	// The result must never be stored, logged, or returned in an error.
	Detokenize(ctx context.Context, token, caller, reason string) (string, error)
}

// CardMetadata is everything the payments service is permitted to know about a
// card. Note the absence of a number field — the type makes exposure
// structurally impossible, not merely discouraged.
type CardMetadata struct {
	Brand       string `json:"brand"`
	Last4       string `json:"last4"`
	BIN         string `json:"bin"`
	Fingerprint string `json:"fingerprint"`
	ExpMonth    int    `json:"exp_month"`
	ExpYear     int    `json:"exp_year"`

	// Behavior is how the card was entered, observed by the vault's frame and
	// attested by the vault. It arrives with the token rather than from the
	// merchant, so a compromised integration cannot claim the typing looked
	// human.
	Behavior map[string]any `json:"behavior,omitempty"`
}

// RiskAssessor scores a charge before it reaches the processor (§14.1).
//
// An interface so this package does not depend on the risk package, and so
// tests can drive specific verdicts. A nil assessor means risk checking is
// disabled entirely — valid for tests, never for production.
type RiskAssessor interface {
	Assess(ctx context.Context, txn risk.Transaction) risk.Decision
	RecordOutcome(ctx context.Context, txn risk.Transaction, declined bool)
}

type Service struct {
	pool  *pgxpool.Pool
	bank  *BankClient
	vault VaultClient
	risk  RiskAssessor
	// feeBps is the platform fee in basis points (100 bps = 1%).
	feeBps int64
}

func NewService(pool *pgxpool.Pool, bank *BankClient, vaultClient VaultClient, assessor RiskAssessor) *Service {
	return &Service{pool: pool, bank: bank, vault: vaultClient, risk: assessor, feeBps: 290}
}

type ChargeInput struct {
	MerchantID  uuid.UUID
	AmountCents int64
	Currency    string
	// PaymentToken references a card held in the vault. The payments service
	// never accepts a raw card number — that is the whole point of §2.4.
	PaymentToken string
	Description  string
	Metadata     map[string]any

	// Fraud signals captured at the edge (§14.5).
	DeviceFingerprint string
	IPAddress         string
	// UserAgent is used only to classify desktop vs mobile for the model. It is
	// deliberately not stored — a full User-Agent is a fingerprinting surface,
	// and the one bit we need does not justify keeping the rest.
	UserAgent string

	SimulateOutcome string // test-mode only, forwarded to the bank simulator
}

type Charge struct {
	ID             uuid.UUID      `json:"id"`
	Object         string         `json:"object"`
	AmountCents    int64          `json:"amount"`
	Currency       string         `json:"currency"`
	Status         string         `json:"status"`
	FailureCode    string         `json:"failure_code,omitempty"`
	FailureMessage string         `json:"failure_message,omitempty"`
	CardLast4      string         `json:"card_last4,omitempty"`
	CardBrand      string         `json:"card_brand,omitempty"`
	LedgerTxnID    *uuid.UUID     `json:"ledger_transaction_id,omitempty"`
	ProcessorRef   string         `json:"processor_reference,omitempty"`
	Metadata       map[string]any `json:"metadata"`
	CreatedAt      time.Time      `json:"created_at"`
}

var (
	ErrInvalidAmount   = errors.New("payments: amount must be positive")
	ErrMerchantUnknown = errors.New("payments: merchant not found")
	ErrMissingToken    = errors.New("payments: a payment token is required")

	// ErrTokenUnusable covers a token that is missing, expired, or already
	// consumed. They are deliberately collapsed into one error so the API
	// cannot be used to distinguish between them — telling a caller that a
	// token exists but was already spent leaks more than it helps.
	ErrTokenUnusable = errors.New("payments: payment token is unusable")
)

// CreateCharge runs the full charge flow.
//
// The sequencing here is the crux of the whole system, and it is deliberate.
// THREE transactions, not two — the comment previously said two and was wrong:
//
//	tx1  claim the idempotency key, and NOTHING else, then commit
//	     — a retry must never re-execute a precondition the first attempt may
//	       have changed, and the single-use vault token is exactly that
//	     ↓
//	     vault metadata, then risk scoring
//	     — neither writes to the database or has a side effect outside this
//	       process, so a crash here leaves nothing dangling
//	     ↓
//	tx2  insert the charge as 'pending', carrying the risk verdict
//	     — durable before anything leaves this system, so a crash mid-flight
//	       leaves a record to reconcile against rather than a charge nobody
//	       knows happened
//	     ↓
//	     detokenize, then call the bank, both OUTSIDE any transaction
//	     — a network call inside one holds row locks for the duration of
//	       somebody else's latency; under load that alone exhausts the pool
//	     ↓
//	tx3  ledger entries, outbox row, charge status and idempotency response —
//	     all in ONE transaction, so money moving and the event announcing it
//	     can never diverge (§4.2 step 5, §22.1)
//
// See docs/PAYMENT_TRANSACTION_FLOW.md for the statement-level map.
func (s *Service) CreateCharge(ctx context.Context, in ChargeInput, idemKey, requestHash string, rawBody []byte) (*Charge, int, error) {
	amount := money.New(in.AmountCents, in.Currency)
	if err := amount.Validate(); err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrInvalidAmount, err)
	}

	if in.PaymentToken == "" {
		return nil, 0, ErrMissingToken
	}

	chargeID := uuid.New()

	// --- tx1: claim the idempotency key, and nothing else ---------------------
	//
	// This MUST be the first thing that happens, before any validation or
	// lookup that could fail. The reason is subtle and was a real bug here: a
	// successful charge consumes its single-use vault token, so on a retry the
	// token is legitimately gone. If the token were checked first, the retry
	// would fail with "invalid token" instead of replaying the original
	// response — the exact double-charge-adjacent confusion idempotency exists
	// to prevent.
	//
	// The general rule: a retry must never re-execute a precondition that the
	// first attempt may have changed.
	var (
		idemRecordID uuid.UUID
		replay       *idempotency.Record
	)
	err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		outcome, rec, err := idempotency.Begin(ctx, tx, in.MerchantID, idemKey, "POST /v1/charges", requestHash)
		if err != nil {
			return err
		}
		if outcome == idempotency.OutcomeReplay {
			replay = rec
			return nil
		}
		idemRecordID = rec.ID
		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	// A completed key replays its stored response verbatim, without re-charging
	// and without touching the vault at all.
	//
	if replay != nil {
		var prior Charge
		if len(replay.ResponseBody) > 0 {
			if err := json.Unmarshal(replay.ResponseBody, &prior); err != nil {
				return nil, 0, fmt.Errorf("payments: decode replayed response: %w", err)
			}
		}
		status := replay.ResponseStatus
		if status == 0 {
			status = 200
		}
		return &prior, status, nil
	}

	// --- card metadata, outside any transaction -------------------------------
	//
	// Brand, last4, BIN, fingerprint — never a PAN. A network call, so it is
	// deliberately not inside a transaction: holding row locks for the duration
	// of someone else's latency exhausts the connection pool under load.
	card, err := s.vault.Metadata(ctx, in.PaymentToken)
	if err != nil {
		// Only a token that is genuinely unusable gets a terminal record.
		//
		// The distinction matters and was originally missing. A vault pod
		// restarting returns 503, which is NOT ErrTokenUnusable — but the old
		// code recorded any error as a permanent "invalid_payment_token"
		// failure. The client's retry, which is exactly the right thing for
		// them to do, would then replay that stored 400 forever: a valid token
		// permanently unusable under that key, and a merchant told their token
		// was bad when the vault had simply blinked.
		if errors.Is(err, ErrTokenUnusable) {
			if ferr := s.failClaimed(ctx, chargeID, idemRecordID, in, amount,
				"invalid_payment_token", "The payment token is invalid, expired, or has already been used."); ferr != nil {
				return nil, 0, ferr
			}
			return nil, 0, err
		}

		// Transient. Release the claim so an immediate retry can proceed
		// normally rather than getting 409 until the lock goes stale.
		if rerr := s.releaseClaim(ctx, idemRecordID); rerr != nil {
			slog.Error("payments: failed to release idempotency claim",
				"charge_id", chargeID, "error", rerr)
		}
		return nil, 0, fmt.Errorf("payments: vault unavailable: %w", err)
	}

	// --- risk assessment (§14.1) ----------------------------------------------
	//
	// Sits after the idempotency claim and before the processor call: a
	// declined-for-risk charge must never reach the bank, and a retry must
	// replay the decline rather than re-score it (a second scoring could
	// legitimately reach a different verdict as velocity counters move, which
	// would make the same request non-idempotent).
	//
	// Scored BEFORE the charge row is written, which is a change from the
	// original ordering. Nothing here touches the database or leaves the system
	// with a side effect — it reads Redis counters and calls a scoring service
	// that returns a number — so there is nothing a crash at this point could
	// leave dangling. Moving it earlier lets the verdict be written INTO the
	// charge row rather than UPDATEd onto it a moment later.
	//
	// The invariant that actually matters is unchanged: the charge row is
	// durable before anything reaches the bank.
	//
	// This card's spending history, for the model's amt_zscore_card feature:
	// "is this charge unusual FOR THIS CARD". A £900 charge is unremarkable on
	// a card that averages £850 and alarming on one that averages £12, and
	// nothing else in the feature set expresses that.
	//
	// It was previously left at zero on every charge, so the feature was a
	// constant and contributed nothing. One indexed aggregate against
	// idx_charges_fingerprint, and a failure is non-fatal — a missing signal
	// must never fail a payment.
	avgCents, stdCents := s.cardAmountStats(ctx, card.Fingerprint)

	riskTxn := risk.Transaction{
		MerchantID:      in.MerchantID.String(),
		AmountCents:     in.AmountCents,
		Currency:        amount.Currency,
		CardFingerprint: card.Fingerprint,
		CardBIN:         card.BIN,
		CardBrand:       card.Brand,
		IPAddress:       in.IPAddress,
		DeviceID:        in.DeviceFingerprint,
		Timestamp:       time.Now().UTC(),
		// Derived from the User-Agent. The model's vocabulary for this is
		// exactly {desktop, mobile}, which a User-Agent can answer honestly —
		// unlike ProductCD or addr1, whose vocabularies are Vesta's anonymized
		// codes that nothing outside their dataset can produce.
		DeviceType:         deviceTypeFrom(in.UserAgent),
		CardAvgAmountCents: avgCents,
		CardStdAmountCents: stdCents,
		// How the card was entered, attested by the vault rather than supplied
		// by the merchant — a compromised integration cannot claim the typing
		// looked human.
		Behavior: behaviorFrom(card.Behavior),
	}

	var decision risk.Decision
	if s.risk != nil {
		decision = s.risk.Assess(ctx, riskTxn)
	}

	// --- tx2: record intent ---------------------------------------------------
	//
	// A durable record before anything leaves this system, so a crash mid-flight
	// leaves something for reconciliation rather than a charge nobody knows about.
	//
	// The risk VERDICT is now written by this INSERT rather than UPDATEd onto
	// the row a moment later. That closes a real window: previously a charge
	// existed for a while carrying no verdict, and if the UPDATE failed it
	// stayed that way — a decline nobody could explain afterwards.
	//
	// The detailed risk_assessments row is written separately, below and
	// outside this transaction, on purpose. See the comment there.
	if err := db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		summary, level, rules, err := riskSummary(decision)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO charges
				(id, merchant_id, amount_cents, currency, status,
				 card_fingerprint, card_last4, card_brand, card_bin,
				 device_fingerprint, ip_address, idempotency_key, metadata,
				 payment_token, risk_score, risk_level, risk_rules_fired)
			VALUES ($1,$2,$3,$4,'pending',$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
			ON CONFLICT (id) DO NOTHING`,
			chargeID, in.MerchantID, in.AmountCents, amount.Currency,
			card.Fingerprint, card.Last4, card.Brand, card.BIN,
			nullIfEmpty(in.DeviceFingerprint), nullIfEmpty(in.IPAddress),
			idemKey, orEmptyMap(in.Metadata), in.PaymentToken,
			summary, level, rules,
		); err != nil {
			return fmt.Errorf("payments: insert charge: %w", err)
		}

		return nil
	}); err != nil {
		return nil, 0, err
	}

	if s.risk != nil {
		// Deliberately NOT in the transaction above.
		//
		// The assessment is an audit record, not a gate: failing to store it
		// must not fail the customer's payment. Putting it in tx2 would make an
		// audit-write failure decline a legitimate charge, which is the wrong
		// trade — and the part that genuinely must not be lost, the verdict
		// itself, is already committed on the charge row.
		if err := s.recordRisk(ctx, chargeID, in.MerchantID, card, riskTxn, decision); err != nil {
			slog.Warn("payments: failed to record risk assessment",
				"charge_id", chargeID, "error", err)
		}

		if decision.Level == risk.LevelHigh {
			slog.Info("payments: charge declined by risk engine",
				"charge_id", chargeID, "score", decision.Score,
				"rules", decision.RulesFired, "model_skipped", decision.ModelSkipped)

			// Counts as a decline for velocity: a fraudster's blocked attempts
			// are exactly the signal that catches the next one.
			s.risk.RecordOutcome(ctx, riskTxn, true)

			// A deliberately vague message. Telling the cardholder which rule
			// caught them tells a fraudster what to change; the merchant sees
			// the detail on the charge record and in the dashboard.
			if err := s.finalize(ctx, chargeID, idemRecordID, StatusFailed,
				"risk_declined", "This payment was declined by our fraud checks.",
				"", nil, in, card, amount); err != nil {
				return nil, 0, err
			}
			return &Charge{
				ID: chargeID, Object: "charge", AmountCents: in.AmountCents,
				Currency: amount.Currency, Status: StatusFailed,
				FailureCode: "risk_declined", FailureMessage: "This payment was declined by our fraud checks.",
				CardLast4: card.Last4, CardBrand: card.Brand,
				Metadata: orEmptyMapGo(in.Metadata), CreatedAt: time.Now().UTC(),
			}, 402, nil
		}
	}

	// A medium verdict currently proceeds with the assessment recorded. The
	// faithful behaviour is a 3DS step-up (§16), which needs the requires_action
	// state machine — the idempotency layer already has the status for it.

	// --- the PAN's only appearance outside the vault --------------------------
	//
	// Fetched here, immediately before submission, and held in a local variable
	// that goes out of scope as soon as the bank call returns. It is never
	// assigned to a struct field that outlives this function, never logged, and
	// never included in an error. A single-use token is consumed by this call,
	// so a replay of the same token cannot charge the card twice.
	pan, err := s.vault.Detokenize(ctx, in.PaymentToken, "payments-api",
		fmt.Sprintf("charge %s", chargeID))
	if err != nil {
		// Vault failures are clean failures: nothing was sent to the processor,
		// so this is unambiguous and safe to report as failed.
		if ferr := s.finalize(ctx, chargeID, idemRecordID, StatusFailed,
			"tokenization_error", "The payment token could not be used.", "",
			nil, in, card, amount); ferr != nil {
			return nil, 0, ferr
		}
		return nil, 0, err
	}

	// --- bank call, outside any transaction -----------------------------------
	bankResp, bankErr := s.bank.Charge(ctx, BankChargeRequest{
		// Deriving the downstream key from ours makes the processor's own
		// idempotency cover our retries too (§24.1).
		IdempotencyKey: fmt.Sprintf("%s:%s", in.MerchantID, idemKey),
		AmountCents:    in.AmountCents,
		Currency:       amount.Currency,
		CardNumber:     pan,
		CardExpMonth:   card.ExpMonth,
		CardExpYear:    card.ExpYear,
	}, in.SimulateOutcome)
	pan = "" // drop the reference as early as possible

	// The ambiguous case: park the charge for reconciliation instead of
	// guessing. Deliberately NOT marked failed — the money may well have moved.
	if bankErr != nil && errors.Is(bankErr, ErrAmbiguous) {
		slog.Warn("charge outcome ambiguous, deferring to reconciliation",
			"charge_id", chargeID, "error", bankErr)

		if err := s.finalize(ctx, chargeID, idemRecordID, StatusRequiresReconciliation,
			"", "", "", nil, in, card, amount); err != nil {
			return nil, 0, err
		}
		return &Charge{
			ID: chargeID, Object: "charge", AmountCents: in.AmountCents,
			Currency: amount.Currency, Status: StatusRequiresReconciliation,
			CardLast4: card.Last4, CardBrand: card.Brand,
			Metadata: orEmptyMapGo(in.Metadata), CreatedAt: time.Now().UTC(),
		}, 202, nil
	}

	// Provably never landed — safe to fail cleanly and let the client retry.
	if bankErr != nil {
		if err := s.finalize(ctx, chargeID, idemRecordID, StatusFailed,
			"processor_unreachable", bankErr.Error(), "", nil, in, card, amount); err != nil {
			return nil, 0, err
		}
		return &Charge{
			ID: chargeID, Object: "charge", AmountCents: in.AmountCents,
			Currency: amount.Currency, Status: StatusFailed,
			FailureCode: "processor_unreachable", FailureMessage: "The payment processor could not be reached.",
			CardLast4: card.Last4, CardBrand: card.Brand,
			Metadata: orEmptyMapGo(in.Metadata), CreatedAt: time.Now().UTC(),
		}, 502, nil
	}

	// Feed the outcome back into velocity, whatever it was.
	if s.risk != nil {
		s.risk.RecordOutcome(ctx, riskTxn, bankErr != nil || (bankResp != nil && bankResp.Status == "declined"))
	}

	if bankResp.Status == "declined" {
		if err := s.finalize(ctx, chargeID, idemRecordID, StatusFailed,
			bankResp.DeclineCode, bankResp.DeclineMessage, bankResp.ProcessorReference,
			nil, in, card, amount); err != nil {
			return nil, 0, err
		}
		return &Charge{
			ID: chargeID, Object: "charge", AmountCents: in.AmountCents,
			Currency: amount.Currency, Status: StatusFailed,
			FailureCode: bankResp.DeclineCode, FailureMessage: bankResp.DeclineMessage,
			CardLast4: card.Last4, CardBrand: card.Brand,
			ProcessorRef: bankResp.ProcessorReference,
			Metadata:     orEmptyMapGo(in.Metadata), CreatedAt: time.Now().UTC(),
		}, 402, nil
	}

	// --- tx2: authorized. Post the ledger and finalize atomically -------------
	ledgerTxnID := uuid.New()
	if err := s.finalize(ctx, chargeID, idemRecordID, StatusSucceeded, "", "",
		bankResp.ProcessorReference, &ledgerTxnID, in, card, amount); err != nil {
		return nil, 0, err
	}

	return &Charge{
		ID: chargeID, Object: "charge", AmountCents: in.AmountCents,
		Currency: amount.Currency, Status: StatusSucceeded,
		CardLast4: card.Last4, CardBrand: card.Brand,
		LedgerTxnID: &ledgerTxnID, ProcessorRef: bankResp.ProcessorReference,
		Metadata: orEmptyMapGo(in.Metadata), CreatedAt: time.Now().UTC(),
	}, 200, nil
}

// recordRisk stores the assessment, both as a summary on the charge and as a
// durable row for later review (§14.5, §14.4).
//
// Written outside the finalize transaction deliberately: this is an audit
// record, and failing to store it must never roll back or block the charge it
// describes.
//
// The full evidence is snapshotted here because the vault token it came from
// is purged within 15 minutes. A chargeback arriving three weeks later needs
// to know what the typing looked like at the time, and by then the token is
// long gone.
// riskSummary flattens a decision into the three columns the charges row keeps,
// so the verdict can be written by the INSERT rather than UPDATEd on afterwards.
//
// Returns NULLs when no assessment was made — risk checking disabled, or the
// zero Decision. charges.risk_level is CHECKed against ('low','medium','high'),
// so writing the empty string violates the constraint and fails the charge. The
// absence of a verdict has to be recorded as absence, not as a fourth level.
//
// The rule score is 0-100; risk_score is NUMERIC(5,4), so the model probability
// is stored when present and the rule score normalized otherwise.
func riskSummary(d risk.Decision) (score, level, rules any, err error) {
	if d.Level == "" {
		// score and level are nullable; risk_rules_fired is NOT NULL DEFAULT
		// '[]', so "no rules fired" is an empty array rather than an absence.
		return nil, nil, []byte("[]"), nil
	}

	encoded, err := json.Marshal(d.RulesFired)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("payments: marshal fired rules: %w", err)
	}

	normalized := d.Score / 100
	if d.ModelScore != nil {
		normalized = *d.ModelScore
	}
	if normalized > 1 {
		normalized = 1
	}
	return normalized, string(d.Level), encoded, nil
}

func (s *Service) recordRisk(ctx context.Context, chargeID, merchantID uuid.UUID,
	card *CardMetadata, txn risk.Transaction, d risk.Decision) error {

	_, _, rules, err := riskSummary(d)
	if err != nil {
		return err
	}

	action := "allowed"
	switch d.Level {
	case risk.LevelHigh:
		action = "declined"
	case risk.LevelMedium:
		// Currently proceeds — 3DS step-up (§16) is not built. Recorded as
		// challenged so the gap is visible in the data rather than looking
		// like a clean approval.
		action = "challenged"
	}

	velocity, err := json.Marshal(txn.Velocity)
	if err != nil {
		return fmt.Errorf("payments: marshal velocity: %w", err)
	}

	// ON CONFLICT DO NOTHING because a retry replays rather than re-scoring —
	// a second row here would mean the idempotency guarantee had broken.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO risk_assessments
			(charge_id, merchant_id, level, rule_score, rules_fired, reason,
			 model_score, model_version, model_skipped,
			 behavior, velocity, card_fingerprint, card_bin,
			 ip_address, device_id, action_taken, latency_ms)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		ON CONFLICT (charge_id) DO NOTHING`,
		chargeID, merchantID, string(d.Level), d.Score, rules, nullIfEmpty(d.Reason),
		d.ModelScore, nullIfEmpty(d.ModelVersion), d.ModelSkipped,
		orEmptyMap(card.Behavior), velocity, nullIfEmpty(card.Fingerprint),
		nullIfEmpty(card.BIN), nullIfEmpty(txn.IPAddress), nullIfEmpty(txn.DeviceID),
		action, d.LatencyMs,
	); err != nil {
		return fmt.Errorf("payments: record risk assessment: %w", err)
	}
	return nil
}

// releaseClaim abandons a claim after a transient failure, so an immediate
// retry starts fresh rather than replaying a failure that says nothing about
// the request.
func (s *Service) releaseClaim(ctx context.Context, idemRecordID uuid.UUID) error {
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		return idempotency.Release(ctx, tx, idemRecordID)
	})
}

// failClaimed marks a claimed idempotency key as failed when the request cannot
// proceed past validation — before any charge row exists.
//
// Without this, a request that claims a key and then fails a precondition would
// leave the key stuck in 'processing' until its lock went stale, so the client's
// immediate retry would get 409 instead of the actual error. Writing a terminal
// response makes the failure replayable, which is what the client needs to see.
func (s *Service) failClaimed(
	ctx context.Context,
	chargeID, idemRecordID uuid.UUID,
	in ChargeInput,
	amount money.Amount,
	code, message string,
) error {
	response := Charge{
		ID: chargeID, Object: "charge", AmountCents: in.AmountCents,
		Currency: amount.Currency, Status: StatusFailed,
		FailureCode: code, FailureMessage: message,
		Metadata: orEmptyMapGo(in.Metadata), CreatedAt: time.Now().UTC(),
	}
	body, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("payments: marshal failure response: %w", err)
	}

	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		return idempotency.Complete(ctx, tx, idemRecordID, idempotency.StatusFailed, 400, body)
	})
}

// finalize commits the terminal state of a charge in one transaction: ledger
// entries (when the charge succeeded), charge row, outbox event, and the
// idempotency response. Committing these separately is precisely the
// inconsistency the outbox pattern exists to prevent (§22.1).
func (s *Service) finalize(
	ctx context.Context,
	chargeID, idemRecordID uuid.UUID,
	status, failureCode, failureMessage, processorRef string,
	ledgerTxnID *uuid.UUID,
	in ChargeInput,
	card *CardMetadata,
	amount money.Amount,
) error {
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if status == StatusSucceeded && ledgerTxnID != nil {
			if err := s.postChargeLedger(ctx, tx, *ledgerTxnID, in, amount); err != nil {
				return err
			}
		}

		_, err := tx.Exec(ctx, `
			UPDATE charges
			SET status = $2, failure_code = $3, failure_message = $4,
			    processor_reference = $5, ledger_transaction_id = $6, updated_at = now()
			WHERE id = $1`,
			chargeID, status, nullIfEmpty(failureCode), nullIfEmpty(failureMessage),
			nullIfEmpty(processorRef), ledgerTxnID,
		)
		if err != nil {
			return fmt.Errorf("payments: update charge: %w", err)
		}

		response := Charge{
			ID: chargeID, Object: "charge", AmountCents: in.AmountCents,
			Currency: amount.Currency, Status: status,
			FailureCode: failureCode, FailureMessage: failureMessage,
			CardLast4: card.Last4, CardBrand: card.Brand,
			LedgerTxnID: ledgerTxnID, ProcessorRef: processorRef,
			Metadata: orEmptyMapGo(in.Metadata), CreatedAt: time.Now().UTC(),
		}
		body, err := json.Marshal(response)
		if err != nil {
			return fmt.Errorf("payments: marshal charge response: %w", err)
		}

		eventType := "payment.succeeded"
		httpStatus := 200
		idemStatus := idempotency.StatusCompleted
		switch status {
		case StatusFailed:
			eventType, httpStatus, idemStatus = "payment.failed", 402, idempotency.StatusFailed
		case StatusRequiresReconciliation:
			eventType, httpStatus = "payment.requires_reconciliation", 202
		}

		// Same transaction as the ledger write — this is the outbox guarantee.
		if _, err := tx.Exec(ctx, `
			INSERT INTO outbox_events (aggregate_id, event_type, payload)
			VALUES ($1, $2, $3)`,
			chargeID, eventType, body,
		); err != nil {
			return fmt.Errorf("payments: insert outbox event: %w", err)
		}

		return idempotency.Complete(ctx, tx, idemRecordID, idemStatus, httpStatus, body)
	})
}

// postChargeLedger writes the double-entry legs for a successful charge.
//
// Three legs, not two: the customer owes the money (debit receivable), the
// merchant is credited their net, and the platform is credited its fee. All
// three balance to zero, which is what the deferred constraint verifies at
// COMMIT.
func (s *Service) postChargeLedger(ctx context.Context, tx pgx.Tx, txnID uuid.UUID, in ChargeInput, amount money.Amount) error {
	fee := in.AmountCents * s.feeBps / 10_000
	net := in.AmountCents - fee

	receivable, err := ledger.EnsureAccount(ctx, tx, in.MerchantID, ledger.AccountCustomerReceivable, amount.Currency)
	if err != nil {
		return err
	}
	merchantBalance, err := ledger.EnsureAccount(ctx, tx, in.MerchantID, ledger.AccountMerchantBalance, amount.Currency)
	if err != nil {
		return err
	}
	platformFees, err := ledger.EnsureAccount(ctx, tx, uuid.Nil, ledger.AccountPlatformFees, amount.Currency)
	if err != nil {
		return err
	}

	legs := []ledger.Leg{
		ledger.Debit(receivable, money.New(in.AmountCents, amount.Currency), ledger.EntryCharge),
		ledger.Credit(merchantBalance, money.New(net, amount.Currency), ledger.EntryCharge),
	}
	// A zero fee would violate the amount_cents > 0 constraint, so the leg is
	// only written when there is actually a fee to record.
	if fee > 0 {
		legs = append(legs, ledger.Credit(platformFees, money.New(fee, amount.Currency), ledger.EntryFee))
	}

	return ledger.Post(ctx, tx, ledger.Transaction{ID: txnID, Legs: legs})
}

// GetCharge fetches a charge belonging to a merchant.
func (s *Service) GetCharge(ctx context.Context, merchantID, chargeID uuid.UUID) (*Charge, error) {
	var (
		c            Charge
		failCode     *string
		failMsg      *string
		last4, brand *string
		procRef      *string
		ledgerTxn    *uuid.UUID
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, amount_cents, currency, status, failure_code, failure_message,
		       card_last4, card_brand, processor_reference, ledger_transaction_id,
		       metadata, created_at
		FROM charges
		WHERE id = $1 AND merchant_id = $2`,
		chargeID, merchantID,
	).Scan(&c.ID, &c.AmountCents, &c.Currency, &c.Status, &failCode, &failMsg,
		&last4, &brand, &procRef, &ledgerTxn, &c.Metadata, &c.CreatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("payments: get charge: %w", err)
	}

	c.Object = "charge"
	c.FailureCode = deref(failCode)
	c.FailureMessage = deref(failMsg)
	c.CardLast4 = deref(last4)
	c.CardBrand = deref(brand)
	c.ProcessorRef = deref(procRef)
	c.LedgerTxnID = ledgerTxn
	if c.Metadata == nil {
		c.Metadata = map[string]any{}
	}
	return &c, nil
}

// behaviorFrom converts the vault's JSON aggregates into the typed struct the
// rules consume. Anything missing stays nil, which the rules treat as "unknown"
// rather than "suspicious" — a non-browser API caller supplies none of this and
// must not be penalised for it.
// cardAmountStats returns this card's mean and standard deviation of
// successful charge amounts, in minor units.
//
// Bounded to 90 days and 500 rows: a card's "usual" amount is a recent
// property, and an unbounded aggregate would get slower for exactly the cards
// that are used most. Both are zero for a card we have not seen before, which
// the model reads as "no history" — the correct answer for a first charge
// rather than a fabricated one.
func (s *Service) cardAmountStats(ctx context.Context, fingerprint string) (avg, std int64) {
	if fingerprint == "" {
		return 0, 0
	}
	var avgF, stdF *float64
	err := s.pool.QueryRow(ctx, `
		SELECT avg(amount_cents), coalesce(stddev_samp(amount_cents), 0)
		FROM (
			SELECT amount_cents FROM charges
			WHERE card_fingerprint = $1
			  AND status = 'succeeded'
			  AND created_at > now() - INTERVAL '90 days'
			ORDER BY created_at DESC
			LIMIT 500
		) recent`, fingerprint).Scan(&avgF, &stdF)

	if err != nil {
		// Non-fatal by design. This is one input to a score, and the charge
		// path must survive losing it — the same reasoning that makes the whole
		// risk engine fail open.
		slog.Warn("payments: card amount stats unavailable",
			"error", err, "trace_id", httpx.TraceID(ctx))
		return 0, 0
	}
	if avgF != nil {
		avg = int64(*avgF)
	}
	if stdF != nil {
		std = int64(*stdF)
	}
	return avg, std
}

// deviceTypeFrom classifies a User-Agent as mobile or desktop.
//
// Crude on purpose. The model's vocabulary has exactly two values, so any
// further precision would be thrown away at encoding time, and a heavyweight
// UA-parsing dependency on the charge path would buy nothing. An unrecognised
// or absent User-Agent returns "" rather than guessing "desktop" — a wrong
// value is worse than a missing one, because the model reads "" as unknown and
// a wrong label as fact.
func deviceTypeFrom(ua string) string {
	if ua == "" {
		return ""
	}
	lower := strings.ToLower(ua)
	for _, marker := range []string{"mobi", "android", "iphone", "ipad", "ipod", "windows phone"} {
		if strings.Contains(lower, marker) {
			return "mobile"
		}
	}
	// Only claim desktop for something that actually looks like a browser. A
	// server-side integration calling the API has a Go/curl User-Agent and no
	// device at all, and labelling that "desktop" would be a lie the model
	// would learn from.
	if strings.Contains(lower, "mozilla") || strings.Contains(lower, "safari") {
		return "desktop"
	}
	return ""
}

func behaviorFrom(raw map[string]any) risk.Behavior {
	if len(raw) == 0 {
		return risk.Behavior{}
	}

	num := func(key string) *float64 {
		if v, ok := raw[key].(float64); ok {
			return &v
		}
		return nil
	}
	b := risk.Behavior{Present: true}

	if v, ok := raw["pasted"].(bool); ok {
		b.Pasted = v
	}
	if v, ok := raw["number_keystrokes"].(float64); ok {
		b.NumberKeystrokes = int(v)
	}
	if fields, ok := raw["pasted_fields"].([]any); ok {
		for _, f := range fields {
			if s, ok := f.(string); ok {
				b.PastedFields = append(b.PastedFields, s)
			}
		}
	}
	b.KeystrokeIntervalCV = num("keystroke_interval_cv")
	b.KeystrokeIntervalMean = num("keystroke_interval_mean_ms")
	b.CorrectionRate = num("correction_rate")
	b.CVCHesitationMs = num("cvc_hesitation_ms")
	b.TotalDurationMs = num("total_duration_ms")
	return b
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func orEmptyMap(m map[string]any) any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func orEmptyMapGo(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
