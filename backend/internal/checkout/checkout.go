// Package checkout implements payment links and the hosted payment page.
//
// The problem it solves: the normal integration requires the merchant to run a
// server, because creating a charge needs the secret key and a secret key in a
// browser is readable by anyone who opens devtools. That rules out every
// merchant who does not have a backend — which is most of the ones a link is
// for.
//
// A payment link moves the server-side half onto us. The customer lands on a
// page PayFlow serves, the card goes into the vault's iframe exactly as it
// always does, and the charge is created here, on the merchant's behalf, using
// the merchant_id stored on the link. The merchant needs no server, no key in
// production, and no code at all.
//
// The cost of that convenience is that the confirm endpoint is PUBLIC. Anyone
// with the URL can reach it, so every assumption the authenticated API makes
// has to be re-earned:
//
//   - the amount comes from the database, never the request body
//   - one checkout session is one attempt, and its id is the idempotency key
//   - a used session is refused rather than reused, so a retry with a different
//     card cannot collide with the first attempt's stored response
package checkout

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/Akhilesh-Vangala/payflow/internal/idempotency"
	"github.com/Akhilesh-Vangala/payflow/internal/payments"
)

var (
	ErrLinkNotFound    = errors.New("checkout: no such payment link")
	ErrSessionNotFound = errors.New("checkout: no such checkout session")
	ErrSessionUsed     = errors.New("checkout: this checkout session has already been used")
	ErrSessionExpired  = errors.New("checkout: this checkout session has expired")
	ErrInvalidAmount   = errors.New("checkout: amount must be a positive integer")
	ErrInvalidURL      = errors.New("checkout: redirect URLs must be absolute http(s) URLs")
)

// Charger is the slice of the payments service this package needs. Narrow on
// purpose: the hosted page can create a charge and nothing else.
type Charger interface {
	CreateCharge(ctx context.Context, in payments.ChargeInput, idemKey, requestHash string, rawBody []byte) (*payments.Charge, int, error)
}

type Service struct {
	pool   *pgxpool.Pool
	charge Charger
}

func NewService(pool *pgxpool.Pool, charger Charger) *Service {
	return &Service{pool: pool, charge: charger}
}

type Link struct {
	ID          uuid.UUID `json:"id"`
	Object      string    `json:"object"`
	Code        string    `json:"code"`
	URL         string    `json:"url"`
	AmountCents int64     `json:"amount"`
	Currency    string    `json:"currency"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	SuccessURL  string    `json:"success_url,omitempty"`
	CancelURL   string    `json:"cancel_url,omitempty"`
	Active      bool      `json:"active"`
	CreatedAt   time.Time `json:"created_at"`

	// Rolled up from the sessions this link produced, so the dashboard can show
	// whether a link is working without a second round trip.
	Payments    int64 `json:"payments"`
	VolumeCents int64 `json:"volume"`
}

type LinkInput struct {
	AmountCents int64
	Currency    string
	Name        string
	Description string
	SuccessURL  string
	CancelURL   string
}

func (s *Service) CreateLink(ctx context.Context, merchantID uuid.UUID, userID *uuid.UUID, in LinkInput) (*Link, error) {
	if in.AmountCents <= 0 {
		return nil, ErrInvalidAmount
	}
	if len(in.Currency) != 3 {
		return nil, fmt.Errorf("checkout: currency must be a 3-letter ISO 4217 code")
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("checkout: a name is required")
	}
	// Validated because these are attacker-influenced values that end up in a
	// browser redirect. A javascript: URL here would be stored XSS against the
	// merchant's own customers.
	for _, u := range []string{in.SuccessURL, in.CancelURL} {
		if u != "" && !isAbsoluteHTTPURL(u) {
			return nil, ErrInvalidURL
		}
	}

	code, err := randomCode()
	if err != nil {
		return nil, err
	}

	l := Link{
		Object: "payment_link", Code: code,
		AmountCents: in.AmountCents, Currency: strings.ToUpper(in.Currency),
		Name: strings.TrimSpace(in.Name), Description: strings.TrimSpace(in.Description),
		SuccessURL: in.SuccessURL, CancelURL: in.CancelURL, Active: true,
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO payment_links
			(merchant_id, code, amount_cents, currency, name, description,
			 success_url, cancel_url, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, created_at`,
		merchantID, code, l.AmountCents, l.Currency, l.Name, l.Description,
		nullIfEmpty(in.SuccessURL), nullIfEmpty(in.CancelURL), userID,
	).Scan(&l.ID, &l.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("checkout: create link: %w", err)
	}
	return &l, nil
}

func (s *Service) ListLinks(ctx context.Context, merchantID uuid.UUID) ([]Link, error) {
	// Only completed sessions count towards the rollup. Counting 'processing'
	// would report money as taken that may not have been.
	rows, err := s.pool.Query(ctx, `
		SELECT l.id, l.code, l.amount_cents, l.currency, l.name, l.description,
		       coalesce(l.success_url, ''), coalesce(l.cancel_url, ''), l.active, l.created_at,
		       count(cs.id) FILTER (WHERE cs.status = 'completed'),
		       coalesce(sum(cs.amount_cents) FILTER (WHERE cs.status = 'completed'), 0)
		FROM payment_links l
		LEFT JOIN checkout_sessions cs ON cs.link_id = l.id
		WHERE l.merchant_id = $1 AND l.archived_at IS NULL
		GROUP BY l.id
		ORDER BY l.created_at DESC
		LIMIT 200`, merchantID)
	if err != nil {
		return nil, fmt.Errorf("checkout: list links: %w", err)
	}
	defer rows.Close()

	links := []Link{}
	for rows.Next() {
		l := Link{Object: "payment_link"}
		if err := rows.Scan(&l.ID, &l.Code, &l.AmountCents, &l.Currency, &l.Name,
			&l.Description, &l.SuccessURL, &l.CancelURL, &l.Active, &l.CreatedAt,
			&l.Payments, &l.VolumeCents); err != nil {
			return nil, fmt.Errorf("checkout: scan link: %w", err)
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// SetActive turns a link on or off. Deactivating is the only "delete" offered:
// a link that has taken money is part of the audit trail for those charges.
func (s *Service) SetActive(ctx context.Context, merchantID, linkID uuid.UUID, active bool) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE payment_links SET active = $3
		WHERE id = $1 AND merchant_id = $2 AND archived_at IS NULL`,
		linkID, merchantID, active)
	if err != nil {
		return fmt.Errorf("checkout: set link active: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLinkNotFound
	}
	return nil
}

// PublicLink is what a stranger opening the URL is allowed to know: the price
// and what it is for. Not the merchant id, not the link's internal id, not how
// many times it has been paid.
type PublicLink struct {
	Code         string `json:"code"`
	AmountCents  int64  `json:"amount"`
	Currency     string `json:"currency"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	BusinessName string `json:"business_name"`
}

func (s *Service) PublicLink(ctx context.Context, code string) (*PublicLink, error) {
	var l PublicLink
	err := s.pool.QueryRow(ctx, `
		SELECT l.code, l.amount_cents, l.currency, l.name, l.description, m.name
		FROM payment_links l
		JOIN merchants m ON m.id = l.merchant_id
		WHERE l.code = $1 AND l.active AND l.archived_at IS NULL`, code,
	).Scan(&l.Code, &l.AmountCents, &l.Currency, &l.Name, &l.Description, &l.BusinessName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLinkNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("checkout: load public link: %w", err)
	}
	return &l, nil
}

type CheckoutSession struct {
	ID          uuid.UUID `json:"id"`
	AmountCents int64     `json:"amount"`
	Currency    string    `json:"currency"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// StartSession records one customer's intent to pay one link.
//
// The amount is COPIED from the link here rather than read at confirm time. A
// merchant who edits the price between a customer loading the page and pressing
// pay must not change what that customer is charged — they agreed to the number
// they saw.
func (s *Service) StartSession(ctx context.Context, code, ip, ua string) (*CheckoutSession, error) {
	var cs CheckoutSession
	err := s.pool.QueryRow(ctx, `
		INSERT INTO checkout_sessions (link_id, merchant_id, amount_cents, currency, ip, user_agent)
		SELECT l.id, l.merchant_id, l.amount_cents, l.currency, $2, $3
		FROM payment_links l
		WHERE l.code = $1 AND l.active AND l.archived_at IS NULL
		RETURNING id, amount_cents, currency, expires_at`,
		code, nullIfEmpty(ip), nullIfEmpty(truncate(ua, 300)),
	).Scan(&cs.ID, &cs.AmountCents, &cs.Currency, &cs.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// INSERT ... SELECT with no matching row inserts nothing, so a missing
		// or deactivated link lands here.
		return nil, ErrLinkNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("checkout: start session: %w", err)
	}
	return &cs, nil
}

// Result is what the hosted page shows the customer.
type Result struct {
	Status     string `json:"status"` // succeeded | processing | failed
	ChargeID   string `json:"charge_id,omitempty"`
	Message    string `json:"message,omitempty"`
	RedirectTo string `json:"redirect_to,omitempty"`
}

type ConfirmInput struct {
	SessionID    uuid.UUID
	PaymentToken string
	IP           string
	DeviceID     string
	UserAgent    string
}

// Confirm exchanges a vault token for a charge on the merchant's behalf.
//
// This runs with NO caller credentials, so the two lines that matter are the
// ones that decide how much and for whom — both read from the session row, both
// unreachable from the request body.
func (s *Service) Confirm(ctx context.Context, in ConfirmInput) (*Result, error) {
	var (
		merchantID uuid.UUID
		linkID     uuid.UUID
		amount     int64
		currency   string
		status     string
		expired    bool
		successURL *string
		linkName   string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT cs.merchant_id, cs.link_id, cs.amount_cents, cs.currency, cs.status,
		       cs.expires_at <= now(), l.success_url, l.name
		FROM checkout_sessions cs
		JOIN payment_links l ON l.id = cs.link_id
		WHERE cs.id = $1`, in.SessionID,
	).Scan(&merchantID, &linkID, &amount, &currency, &status, &expired, &successURL, &linkName)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("checkout: load session: %w", err)
	}

	// A session is one attempt. Refusing a second one is what lets the retry
	// path use a fresh session with a different card without colliding with the
	// first attempt's stored idempotent response — which would otherwise come
	// back as "key reused with a different body" and strand the customer.
	if status != "open" {
		return nil, ErrSessionUsed
	}
	if expired {
		return nil, ErrSessionExpired
	}

	// The idempotency key IS the session id. A double-clicked pay button, a
	// refreshed tab and a retried fetch all carry the same key and produce one
	// charge — the guarantee this whole system is built around, applied to the
	// one flow where the customer, not the merchant, controls the retries.
	idemKey := "cs_" + in.SessionID.String()

	body := []byte(fmt.Sprintf(
		`{"amount":%d,"currency":%q,"payment_token":%q,"checkout_session":%q}`,
		amount, currency, in.PaymentToken, in.SessionID))
	requestHash, err := idempotency.HashRequest(body)
	if err != nil {
		return nil, fmt.Errorf("checkout: hash request: %w", err)
	}

	charge, httpStatus, err := s.charge.CreateCharge(ctx, payments.ChargeInput{
		MerchantID:   merchantID,
		AmountCents:  amount,
		Currency:     currency,
		PaymentToken: in.PaymentToken,
		Description:  linkName,
		Metadata: map[string]any{
			"payment_link_id":     linkID.String(),
			"checkout_session_id": in.SessionID.String(),
			"source":              "payment_link",
		},
		IPAddress:         in.IP,
		DeviceFingerprint: in.DeviceID,
		UserAgent:         in.UserAgent,
	}, idemKey, requestHash, body)

	if err != nil {
		// Which errors may close the session is a money question, not a tidiness
		// one, and getting it wrong here costs a real payment.
		//
		// ErrInFlight means a CONCURRENT request holds the claim and is charging
		// the card right now — a double-clicked pay button. Marking the session
		// failed would race the winner: its own closeSession looks for status
		// 'open', finds 'failed', and leaves a succeeded charge attached to a
		// session that reads as a failure. The link then under-reports money it
		// actually took.
		//
		// An unrecognised error is treated the same way, and for the stronger
		// reason: we do not know how far CreateCharge got. Leaving the session
		// open means a retry reuses the same idempotency key, which replays or
		// resumes the original attempt. Closing it would send the customer to a
		// fresh session and a fresh key — the one path that can charge twice.
		//
		// Only errors that provably happened BEFORE the bank call close it.
		if isTerminal(err) {
			s.closeSession(ctx, in.SessionID, "failed", nil)
		}
		return nil, err
	}

	result := &Result{ChargeID: charge.ID.String()}
	switch {
	case httpStatus == 202 || charge.Status == "requires_reconciliation":
		// Ambiguous. The money may have moved, so this is neither a success to
		// celebrate nor a failure to retry.
		result.Status = "processing"
		result.Message = "Your payment is being confirmed. You will receive an email shortly."
		s.closeSession(ctx, in.SessionID, "processing", &charge.ID)
	case charge.Status == "succeeded":
		result.Status = "succeeded"
		s.closeSession(ctx, in.SessionID, "completed", &charge.ID)
		if successURL != nil {
			result.RedirectTo = *successURL
		}
	default:
		result.Status = "failed"
		result.Message = firstNonEmpty(charge.FailureMessage, "The payment was declined.")
		s.closeSession(ctx, in.SessionID, "failed", &charge.ID)
	}
	return result, nil
}

// isTerminal reports whether a CreateCharge error proves no authorization was
// attempted, and so that the session can be safely closed and retried on a new
// one.
//
// The list is deliberately short and allow-list shaped. Anything not named here
// — including an error we have never seen — is treated as possibly having
// reached the bank.
func isTerminal(err error) bool {
	return errors.Is(err, payments.ErrTokenUnusable) ||
		errors.Is(err, payments.ErrMissingToken) ||
		errors.Is(err, payments.ErrInvalidAmount) ||
		errors.Is(err, payments.ErrMerchantUnknown)
}

// closeSession records the outcome. Deliberately not fatal: the charge is
// already committed and authoritative, and failing the customer's request
// because a status column did not update would be reporting a false negative on
// money that moved.
//
// The status='open' guard makes this safe under concurrency — the first writer
// wins and a later one cannot downgrade a completed session.
func (s *Service) closeSession(ctx context.Context, id uuid.UUID, status string, chargeID *uuid.UUID) {
	_, err := s.pool.Exec(ctx, `
		UPDATE checkout_sessions
		SET status = $2, charge_id = coalesce($3, charge_id),
		    completed_at = now()
		WHERE id = $1 AND status = 'open'`, id, status, chargeID)
	if err != nil {
		// Not returned to the caller — the charge is authoritative and already
		// committed — but never silent either. A session stuck 'open' with a
		// real charge behind it is a reporting discrepancy someone has to see.
		slog.Error("checkout: close session", "error", err,
			"checkout_session_id", id, "intended_status", status)
	}
}

// --- helpers ---------------------------------------------------------------

// randomCode produces the public part of the URL. Random rather than
// sequential: a guessable code would let anyone walk every merchant's catalogue
// and prices.
func randomCode() (string, error) {
	buf := make([]byte, 9)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("checkout: generate code: %w", err)
	}
	return "plink_" + hex.EncodeToString(buf), nil
}

func isAbsoluteHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
