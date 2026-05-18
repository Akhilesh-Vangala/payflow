// Package accounts is merchant self-service: sign up, sign in, and manage the
// API keys that everything else in this system authenticates with.
//
// Before this existed, a merchant got a key by running `make seed`, which is
// fine for a demo and useless for a product. The interesting parts are not the
// CRUD; they are the three places where a shortcut would have been a security
// bug, each marked below:
//
//   - a password is hashed with bcrypt, an API key with SHA-256, and the reason
//     they differ is not preference
//   - login takes the same time whether or not the email exists
//   - a raw key is returned exactly once, from the create call, and is
//     unrecoverable afterwards — including by us
package accounts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/Akhilesh-Vangala/payflow/internal/httpx"
	"golang.org/x/crypto/bcrypt"
)

// SessionTTL is how long a dashboard login lasts. Long enough not to be
// irritating, short enough that a stolen token from a shared machine is not
// permanent. There is no sliding renewal: a session that renews on use never
// expires for the attacker either.
const SessionTTL = 12 * time.Hour

var (
	ErrEmailTaken   = errors.New("accounts: email already registered")
	ErrInvalidLogin = errors.New("accounts: email or password is incorrect")
	ErrWeakPassword = errors.New("accounts: password must be at least 10 characters")
	ErrInvalidEmail = errors.New("accounts: not a valid email address")
	ErrKeyNotFound  = errors.New("accounts: no such API key")
)

// dummyHash is a real bcrypt digest of a value nobody knows. It is compared
// against when the email is not found, so a login attempt for an unknown
// address costs the same ~60ms as one for a known address. Without it, response
// time alone enumerates which of your customers have accounts here.
var dummyHash = []byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy")

type Service struct {
	pool *pgxpool.Pool
	// bcrypt cost. Configurable only so tests are not 12x slower than the code
	// they test; production uses the default.
	bcryptCost int
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, bcryptCost: bcrypt.DefaultCost}
}

type User struct {
	ID         uuid.UUID `json:"id"`
	MerchantID uuid.UUID `json:"merchant_id"`
	Email      string    `json:"email"`
	Name       string    `json:"name"`
	Business   string    `json:"business_name"`
	Country    string    `json:"country"`
	Currency   string    `json:"currency"`
	// PublishableKey is derived from the merchant id, not stored. It is a public
	// identifier rather than a credential — it appears in page source on every
	// merchant's checkout by design — so there is nothing to keep secret, rotate
	// or revoke, and a table for it would imply otherwise.
	PublishableKey string    `json:"publishable_key"`
	CreatedAt      time.Time `json:"created_at"`
}

type Session struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	User      User      `json:"user"`
}

type RegisterInput struct {
	BusinessName string
	Email        string
	Password     string
	Name         string
	Country      string
	Currency     string
	IP           string
	UserAgent    string
}

// Register creates the merchant, its first user, and its first test API key.
//
// All three in one transaction. A merchant with no user cannot be signed into
// and a user with no merchant cannot do anything, so a partial commit here
// produces an account that is broken in a way the person cannot fix themselves.
func (s *Service) Register(ctx context.Context, in RegisterInput) (*Session, string, error) {
	email := strings.TrimSpace(in.Email)
	if _, err := mail.ParseAddress(email); err != nil {
		return nil, "", ErrInvalidEmail
	}
	// Length, and only length. Composition rules ("one symbol, one digit") push
	// people towards Password1! and are no longer recommended by NIST 800-63B.
	if len([]rune(in.Password)) < 10 {
		return nil, "", ErrWeakPassword
	}
	if strings.TrimSpace(in.BusinessName) == "" {
		return nil, "", fmt.Errorf("accounts: business name is required")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), s.bcryptCost)
	if err != nil {
		return nil, "", fmt.Errorf("accounts: hash password: %w", err)
	}

	country := strings.ToUpper(defaultTo(in.Country, "US"))
	currency := strings.ToUpper(defaultTo(in.Currency, "USD"))

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("accounts: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var merchantID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO merchants (name, email, country, currency)
		VALUES ($1, $2, $3, $4)
		RETURNING id`,
		strings.TrimSpace(in.BusinessName), email, country, currency,
	).Scan(&merchantID)
	if isUniqueViolation(err) {
		return nil, "", ErrEmailTaken
	}
	if err != nil {
		return nil, "", fmt.Errorf("accounts: create merchant: %w", err)
	}

	var userID uuid.UUID
	var createdAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO merchant_users (merchant_id, email, password_hash, name)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at`,
		merchantID, email, string(hash), strings.TrimSpace(in.Name),
	).Scan(&userID, &createdAt)
	if isUniqueViolation(err) {
		return nil, "", ErrEmailTaken
	}
	if err != nil {
		return nil, "", fmt.Errorf("accounts: create user: %w", err)
	}

	// The first key is created here rather than left to the merchant, because
	// an account with no key can do nothing and the first thing every new
	// integrator needs is a key to paste into a curl command.
	rawKey, err := insertKey(ctx, tx, merchantID, &userID, "Default test key", "test", "write")
	if err != nil {
		return nil, "", err
	}

	token, expiresAt, err := insertSession(ctx, tx, userID, in.IP, in.UserAgent)
	if err != nil {
		return nil, "", err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, "", fmt.Errorf("accounts: commit: %w", err)
	}

	return &Session{
		Token:     token,
		ExpiresAt: expiresAt,
		User: User{
			ID: userID, MerchantID: merchantID, Email: email,
			Name: strings.TrimSpace(in.Name), Business: strings.TrimSpace(in.BusinessName),
			Country: country, Currency: currency,
			PublishableKey: PublishableKey(merchantID), CreatedAt: createdAt,
		},
	}, rawKey, nil
}

// Login verifies a password and issues a session.
//
// Every failure path returns ErrInvalidLogin and does the same amount of work.
// Distinguishing "no such user" from "wrong password" — in the message, the
// status code, or the timing — turns this endpoint into a way to test whether a
// given address is a customer.
func (s *Service) Login(ctx context.Context, email, password, ip, ua string) (*Session, error) {
	var (
		userID, merchantID uuid.UUID
		hash               string
		u                  User
	)
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.merchant_id, u.password_hash, u.email, u.name, u.created_at,
		       m.name, m.country, m.currency
		FROM merchant_users u
		JOIN merchants m ON m.id = u.merchant_id
		WHERE lower(u.email) = lower($1)`,
		strings.TrimSpace(email),
	).Scan(&userID, &merchantID, &hash, &u.Email, &u.Name, &u.CreatedAt,
		&u.Business, &u.Country, &u.Currency)

	if errors.Is(err, pgx.ErrNoRows) {
		// Spend the same time as a real comparison before answering.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, ErrInvalidLogin
	}
	if err != nil {
		return nil, fmt.Errorf("accounts: lookup user: %w", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return nil, ErrInvalidLogin
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("accounts: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	token, expiresAt, err := insertSession(ctx, tx, userID, ip, ua)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE merchant_users SET last_login_at = now() WHERE id = $1`, userID); err != nil {
		return nil, fmt.Errorf("accounts: touch last login: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("accounts: commit: %w", err)
	}

	u.ID, u.MerchantID = userID, merchantID
	u.PublishableKey = PublishableKey(merchantID)
	return &Session{Token: token, ExpiresAt: expiresAt, User: u}, nil
}

// Logout revokes one session. Revocation rather than deletion keeps the row for
// the session list and for after-the-fact investigation of a takeover.
func (s *Service) Logout(ctx context.Context, rawToken string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE merchant_sessions SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL`,
		httpx.HashSessionToken(rawToken))
	if err != nil {
		return fmt.Errorf("accounts: revoke session: %w", err)
	}
	return nil
}

func (s *Service) Me(ctx context.Context, userID uuid.UUID) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.merchant_id, u.email, u.name, u.created_at,
		       m.name, m.country, m.currency
		FROM merchant_users u
		JOIN merchants m ON m.id = u.merchant_id
		WHERE u.id = $1`, userID,
	).Scan(&u.ID, &u.MerchantID, &u.Email, &u.Name, &u.CreatedAt,
		&u.Business, &u.Country, &u.Currency)
	if err != nil {
		return nil, fmt.Errorf("accounts: load user: %w", err)
	}
	u.PublishableKey = PublishableKey(u.MerchantID)
	return &u, nil
}

// --- API keys --------------------------------------------------------------

type APIKey struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Mode       string     `json:"mode"`
	Scope      string     `json:"scope"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

func (s *Service) ListKeys(ctx context.Context, merchantID uuid.UUID) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, key_prefix, mode, scope, created_at, last_used_at, revoked_at
		FROM api_keys
		WHERE merchant_id = $1
		ORDER BY revoked_at IS NOT NULL, created_at DESC`, merchantID)
	if err != nil {
		return nil, fmt.Errorf("accounts: list keys: %w", err)
	}
	defer rows.Close()

	keys := []APIKey{}
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Mode, &k.Scope,
			&k.CreatedAt, &k.LastUsedAt, &k.RevokedAt); err != nil {
			return nil, fmt.Errorf("accounts: scan key: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// CreateKey mints a key and returns the raw value ONCE.
//
// The raw value is never stored, so this return is the only chance anyone —
// including us — has to see it. That is the point: a support agent who can read
// a merchant's key can also spend a merchant's money.
func (s *Service) CreateKey(ctx context.Context, merchantID uuid.UUID, userID *uuid.UUID,
	name, mode, scope string) (string, error) {

	if mode != "test" && mode != "live" {
		return "", fmt.Errorf("accounts: mode must be test or live")
	}
	if scope != "read" && scope != "write" {
		return "", fmt.Errorf("accounts: scope must be read or write")
	}
	return insertKey(ctx, s.pool, merchantID, userID, defaultTo(strings.TrimSpace(name), "Untitled key"), mode, scope)
}

// RevokeKey is scoped by merchant_id in the WHERE clause, not checked
// afterwards — so a merchant cannot revoke another merchant's key by guessing
// its id, and a missing row and someone else's row are indistinguishable.
func (s *Service) RevokeKey(ctx context.Context, merchantID, keyID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE api_keys SET revoked_at = now()
		WHERE id = $1 AND merchant_id = $2 AND revoked_at IS NULL`, keyID, merchantID)
	if err != nil {
		return fmt.Errorf("accounts: revoke key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// PublishableKey derives the merchant's public identifier.
//
// Deterministic and unstored. It is safe to publish — it appears in the page
// source of every checkout — and it authorises nothing on its own.
func PublishableKey(merchantID uuid.UUID) string {
	return "pk_test_" + strings.ReplaceAll(merchantID.String(), "-", "")
}

// --- helpers ---------------------------------------------------------------

// querier is satisfied by both *pgxpool.Pool and pgx.Tx, so key creation works
// inside registration's transaction and standalone.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func insertKey(ctx context.Context, q querier, merchantID uuid.UUID, userID *uuid.UUID,
	name, mode, scope string) (string, error) {

	raw, err := randomToken("sk_"+mode+"_", 24)
	if err != nil {
		return "", err
	}
	var id uuid.UUID
	err = q.QueryRow(ctx, `
		INSERT INTO api_keys (merchant_id, key_hash, key_prefix, mode, scope, name, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`,
		merchantID, httpx.HashAPIKey(raw), raw[:16], mode, scope, name, userID,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("accounts: create api key: %w", err)
	}
	return raw, nil
}

func insertSession(ctx context.Context, q querier, userID uuid.UUID, ip, ua string) (string, time.Time, error) {
	raw, err := randomToken(httpx.SessionTokenPrefix, 32)
	if err != nil {
		return "", time.Time{}, err
	}
	var expiresAt time.Time
	// The expiry is computed by Postgres from its own now(), for the same
	// reason every other deadline in this system is.
	err = q.QueryRow(ctx, `
		INSERT INTO merchant_sessions (user_id, token_hash, expires_at, ip, user_agent)
		VALUES ($1, $2, now() + $3::interval, $4, $5)
		RETURNING expires_at`,
		userID, httpx.HashSessionToken(raw), SessionTTL.String(), nullIfEmpty(ip), nullIfEmpty(truncate(ua, 300)),
	).Scan(&expiresAt)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("accounts: create session: %w", err)
	}
	return raw, expiresAt, nil
}

func randomToken(prefix string, n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("accounts: generate token: %w", err)
	}
	return prefix + hex.EncodeToString(buf), nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func defaultTo(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
