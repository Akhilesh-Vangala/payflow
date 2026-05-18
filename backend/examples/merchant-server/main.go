// An example of the server a MERCHANT writes to integrate PayFlow.
//
// Not part of PayFlow, and PayFlow does not run it. It exists because a
// checkout page cannot complete a payment on its own: charging requires the
// secret key, and a key in a browser can be read by anyone who opens devtools.
// So something server-side has to hold it, and this is the smallest honest
// version of that.
//
// Note what this process can and cannot do. It holds the secret key and
// charges real money. It never sees a card number, because the card went from
// the customer's browser straight to PayFlow's vault. There is nothing here
// for a PCI assessor to look at.
//
// Signature verification is written out inline rather than imported from
// PayFlow, deliberately — a merchant cannot import PayFlow's internal
// packages, so this is the code they would actually have to write.
//
// Run:
//
//	make up && make seed
//	export PAYFLOW_API_KEY=sk_test_...
//	go run ./examples/merchant-server
package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type server struct {
	payloURL      string
	apiKey        string
	webhookSecret string

	mu     sync.Mutex
	orders []order
	seen   map[string]bool // event ids already handled
}

// order is the merchant's own record. It references the charge by id and
// stores no card data, because none was ever available to store.
type order struct {
	ID        string    `json:"id"`
	ChargeID  string    `json:"charge_id"`
	Amount    int64     `json:"amount"`
	Currency  string    `json:"currency"`
	Status    string    `json:"status"` // paid | pending | failed
	CardBrand string    `json:"card_brand,omitempty"`
	CardLast4 string    `json:"card_last4,omitempty"`
	Failure   string    `json:"failure,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func main() {
	apiKey := os.Getenv("PAYFLOW_API_KEY")
	if apiKey == "" {
		log.Fatal("PAYFLOW_API_KEY is required. Run `make seed` and export the sk_test_ key.")
	}

	s := &server{
		payloURL:      envOr("PAYFLOW_API_URL", "http://localhost:8080"),
		apiKey:        apiKey,
		webhookSecret: envOr("PAYFLOW_WEBHOOK_SECRET", "whsec_demo"),
		seen:          map[string]bool{},
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir("web/demo")))
	mux.HandleFunc("/checkout", s.checkout)
	mux.HandleFunc("/webhooks", s.webhooks)
	mux.HandleFunc("/orders", s.listOrders)

	port := envOr("PORT", "3000")
	log.Printf("demo merchant on http://localhost:%s (charging via %s)", port, s.payloURL)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

type checkoutRequest struct {
	Token    string `json:"payment_token"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// checkout receives a token from the merchant's own page and exchanges it for
// a charge. The secret key is used here and only here.
func (s *server) checkout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req checkoutRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	if req.Token == "" {
		writeJSON(w, 400, map[string]string{"error": "missing payment_token"})
		return
	}
	if req.Currency == "" {
		req.Currency = "USD"
	}

	// A real merchant creates the order in their own database first and uses
	// its id as the idempotency key. The key must identify the ORDER, not the
	// attempt: a double-click or a browser retry then carries the same key and
	// PayFlow charges once. A fresh UUID per attempt defeats the guarantee,
	// and is the most common integration mistake there is.
	orderID := fmt.Sprintf("order_%d", time.Now().UnixNano())

	body, _ := json.Marshal(map[string]any{
		"amount":        req.Amount,
		"currency":      req.Currency,
		"payment_token": req.Token,
		"metadata":      map[string]string{"order_id": orderID},
	})

	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		s.payloURL+"/v1/charges", bytes.NewReader(body))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "internal"})
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+s.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", orderID)

	// Test mode only, so the decline and timeout paths can be exercised by
	// hand. PayFlow ignores this header for live keys, so a merchant cannot
	// steer the real processor with it.
	if outcome := r.Header.Get("X-Simulate-Outcome"); outcome != "" {
		httpReq.Header.Set("X-Simulate-Outcome", outcome)
	}

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(httpReq)
	if err != nil {
		// The charge may have gone through. Record it as pending and reconcile
		// — telling the customer it failed could be wrong, and retrying with a
		// NEW order id could charge them twice.
		log.Printf("charge request failed for %s: %v", orderID, err)
		writeJSON(w, 502, map[string]string{"error": "Could not reach the payment provider."})
		return
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var charge struct {
		ID             string `json:"id"`
		Status         string `json:"status"`
		FailureMessage string `json:"failure_message"`
		CardBrand      string `json:"card_brand"`
		CardLast4      string `json:"card_last4"`
		Error          struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &charge)

	o := order{
		ID: orderID, ChargeID: charge.ID, Amount: req.Amount,
		Currency: req.Currency, CardBrand: charge.CardBrand,
		CardLast4: charge.CardLast4, CreatedAt: time.Now().UTC(),
	}

	switch {
	case resp.StatusCode == 200 && charge.Status == "succeeded":
		o.Status = "paid"
	case resp.StatusCode == 202:
		// PayFlow could not confirm the outcome. DO NOT ship, and do not treat
		// it as a failure — the money may have moved. Reconciliation resolves
		// it and a webhook follows.
		o.Status = "pending"
		o.Failure = "Awaiting confirmation"
	default:
		o.Status = "failed"
		o.Failure = firstNonEmpty(charge.FailureMessage, charge.Error.Message, "Payment declined")
	}

	s.mu.Lock()
	s.orders = append([]order{o}, s.orders...)
	s.mu.Unlock()

	log.Printf("%s -> %s (charge %s)", orderID, o.Status, charge.ID)
	writeJSON(w, resp.StatusCode, o)
}

// webhooks handles PayFlow's asynchronous notifications.
func (s *server) webhooks(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", 400)
		return
	}

	// Verify BEFORE parsing. Anyone can POST to a public webhook URL, so an
	// unverified payload is attacker-controlled input — without this check a
	// stranger can tell you a payment succeeded.
	if !verifySignature(body, r.Header.Get("PayFlow-Signature"), s.webhookSecret, 5*time.Minute) {
		log.Print("rejected webhook: invalid signature")
		http.Error(w, "invalid signature", 400)
		return
	}

	var event struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	_ = json.Unmarshal(body, &event)

	// Delivery is at-least-once, so a retry WILL arrive. Deduplicating on the
	// event id is the merchant's half of that contract.
	s.mu.Lock()
	duplicate := s.seen[event.ID]
	s.seen[event.ID] = true
	s.mu.Unlock()

	if duplicate {
		log.Printf("duplicate webhook ignored: %s", event.ID)
	} else {
		log.Printf("webhook %s (%s)", event.ID, event.Type)
		// Act on it here: mark the order shipped, email a receipt, and so on.
	}

	// 200 either way. Returning an error for a duplicate makes PayFlow retry
	// an event already handled.
	w.WriteHeader(http.StatusOK)
}

// verifySignature checks a PayFlow-Signature header.
//
// Written out rather than imported, because this is code a merchant has to
// write themselves. Header format: t=<unix>,v1=<hex>[,v1=<hex>]
//
// The signed material is "<timestamp>.<raw body>". Including the timestamp is
// what makes a replay detectable — a signature over the body alone would
// verify forever.
func verifySignature(body []byte, header, secret string, tolerance time.Duration) bool {
	var timestamp int64
	var signatures []string

	for _, part := range strings.Split(header, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch key {
		case "t":
			timestamp, _ = strconv.ParseInt(value, 10, 64)
		case "v1":
			// More than one during secret rotation: accept if EITHER matches,
			// or rotation breaks every delivery in flight.
			signatures = append(signatures, value)
		}
	}
	if timestamp == 0 || len(signatures) == 0 {
		return false
	}

	// Reject anything too old. The signature proves authenticity but not
	// freshness; without this a captured webhook could be replayed.
	if age := time.Since(time.Unix(timestamp, 0)); age > tolerance || age < -tolerance {
		return false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", timestamp)
	mac.Write(body)
	expected := mac.Sum(nil)

	for _, candidate := range signatures {
		got, err := hex.DecodeString(candidate)
		if err != nil {
			continue
		}
		// Constant-time. A plain == leaks how much of the signature was
		// correct, which is enough to forge one given enough attempts.
		if hmac.Equal(got, expected) {
			return true
		}
	}
	return false
}

func (s *server) listOrders(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"data": s.orders})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
