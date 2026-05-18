// Load generator for PayFlow.
//
// Written in Go rather than k6 because the interesting question is not "how many
// requests per second" — it is "does exactly-once still hold when the system is
// saturated", which needs assertions against the database afterwards.
//
// Modes:
//
//	health   No auth, no database. Establishes the HTTP-layer ceiling on this
//	         machine, so the gap to -mode=read is the cost of authentication.
//	read     GET the dashboard: auth plus one cursor-paginated query.
//	charge   POST charges through the full money path. Every idempotency key is
//	         used -dupes times concurrently; the assertion that matters is that
//	         charges created equals DISTINCT KEYS, not requests sent.
//
// Load models — see the comment at the dispatch loop. -c alone is a closed
// model; -rate drives an open model and is the one that reveals saturation.
//
//	go run ./benchmarks/loadgen -mode=read   -n=1000000 -c=200
//	go run ./benchmarks/loadgen -mode=charge -n=5000 -rate=200 -c=400 -dupes=1
//
// Traffic shape is deliberately realistic: a distinct Luhn-valid PAN and source
// IP per charge, and keys rotated across merchants. An earlier version reused
// one card from one IP and was correctly declined 16,906 times out of 17,020 by
// the fraud engine — the generator was indistinguishable from card testing.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// runID namespaces this invocation's idempotency keys.
var runID = fmt.Sprintf("load%d", time.Now().Unix())

type stats struct {
	sent     atomic.Int64
	ok       atomic.Int64
	replayed atomic.Int64
	rateLtd  atomic.Int64
	failed   atomic.Int64
	conflict atomic.Int64

	mu      sync.Mutex
	latency []time.Duration
}

func (s *stats) record(d time.Duration) {
	// Sampled: holding a million durations is a gigabyte of nothing useful, and
	// the percentiles are indistinguishable from a 1-in-64 sample at this
	// volume.
	if rand.Intn(64) != 0 {
		return
	}
	s.mu.Lock()
	s.latency = append(s.latency, d)
	s.mu.Unlock()
}

func (s *stats) percentiles() (p50, p95, p99 time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.latency) == 0 {
		return
	}
	sort.Slice(s.latency, func(i, j int) bool { return s.latency[i] < s.latency[j] })
	at := func(q float64) time.Duration {
		i := int(float64(len(s.latency)) * q)
		if i >= len(s.latency) {
			i = len(s.latency) - 1
		}
		return s.latency[i]
	}
	return at(0.50), at(0.95), at(0.99)
}

func main() {
	var (
		mode   = flag.String("mode", "read", "read | charge")
		total  = flag.Int("n", 100000, "total requests")
		conc   = flag.Int("c", 100, "concurrent workers")
		dupes  = flag.Int("dupes", 10, "requests per idempotency key (charge mode)")
		apiURL = flag.String("api", "http://localhost:8080", "payments API base URL")
		vault  = flag.String("vault", "http://localhost:8081", "vault base URL")
		key    = flag.String("key", os.Getenv("PAYFLOW_API_KEY"),
			"sk_test_ API key, or a comma-separated list to spread across merchants")
		rate = flag.Int("rate", 0,
			"target requests/sec (OPEN model). 0 = closed model, driven by -c")
	)
	flag.Parse()

	// Multiple merchants matter for the WRITE path, and not for the reason you
	// might expect. Every charge ends with an UPSERT on the merchant's row in
	// ledger_balances, which takes an exclusive row lock held until the
	// transaction commits. Concurrent charges for ONE merchant therefore
	// serialise on that single row: measured at ~65/s, with individual balance
	// upserts observed blocking for 3.3-6.5 SECONDS under 100 concurrent
	// workers.
	//
	// That is the hot-partition problem, and it is per-merchant rather than
	// global. Driving load through one merchant measures the worst case a
	// single seller could ever create, not the platform's capacity.
	keys := strings.Split(*key, ",")
	for i := range keys {
		keys[i] = strings.TrimSpace(keys[i])
	}

	if *key == "" {
		fmt.Println("need -key or PAYFLOW_API_KEY")
		os.Exit(1)
	}

	// One shared client with a large idle pool. Without this every request opens
	// a fresh TCP connection, and the test measures Windows' ephemeral port
	// table rather than the server.
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        *conc * 2,
			MaxIdleConnsPerHost: *conc * 2,
			MaxConnsPerHost:     *conc * 2,
			IdleConnTimeout:     60 * time.Second,
		},
	}

	s := &stats{}
	work := make(chan int, *conc*4)
	var wg sync.WaitGroup

	started := time.Now()

	// Progress, so a ten-minute run is not a blank screen.
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				n := s.sent.Load()
				el := time.Since(started).Seconds()
				fmt.Printf("  %8d / %d   %7.0f req/s   ok=%d replay=%d 429=%d err=%d\n",
					n, *total, float64(n)/el, s.ok.Load(), s.replayed.Load(),
					s.rateLtd.Load(), s.failed.Load())
			}
		}
	}()

	for range *conc {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				switch *mode {
				case "health":
					// No auth, no database. Establishes the ceiling of the Go
					// HTTP layer on this machine, so the difference against
					// -mode=read is the cost of authentication.
					doGet(client, s, *apiURL+"/healthz", "")
				case "read":
					doRead(client, s, *apiURL, *key)
				case "charge":
					doCharge(client, s, *apiURL, *vault, keys[i%len(keys)], i / *dupes)
				}
			}
		}()
	}

	// Two load models, and which one you use changes what the numbers mean.
	//
	// CLOSED (-c only): N workers each send, wait for a reply, send again. Offered
	// load falls as the server slows, so a saturated system looks like rising
	// latency at flat throughput and never like failure. That is what every
	// measurement in stage 0 used, and it is why throughput looked "flat" rather
	// than "collapsing".
	//
	// OPEN (-rate): requests are issued on a fixed schedule regardless of whether
	// earlier ones have come back. This is how real traffic arrives — customers
	// do not wait for the previous customer — and it is the only model that shows
	// a queue growing without bound. If the server cannot keep up, in-flight work
	// climbs and latency runs away, which is the signal we actually want.
	if *rate > 0 {
		interval := time.Second / time.Duration(*rate)
		ticker := time.NewTicker(interval)
		var dropped atomic.Int64
		for i := range *total {
			<-ticker.C
			select {
			case work <- i:
			default:
				// The queue is full: the system is not keeping up with the
				// offered rate. Recording this rather than blocking is the
				// point — blocking would silently convert the open model back
				// into a closed one and hide the saturation.
				dropped.Add(1)
			}
		}
		ticker.Stop()
		if n := dropped.Load(); n > 0 {
			fmt.Printf("\n  WARNING: %d requests could not be issued at %d/s — "+
				"the system did not keep up with the offered rate\n", n, *rate)
		}
	} else {
		for i := range *total {
			work <- i
		}
	}
	close(work)
	wg.Wait()
	close(done)

	elapsed := time.Since(started)
	p50, p95, p99 := s.percentiles()

	fmt.Printf("\n──────────────────────────────────────────────────\n")
	fmt.Printf("  mode           %s\n", *mode)
	fmt.Printf("  requests       %d in %s\n", s.sent.Load(), elapsed.Round(time.Millisecond))
	fmt.Printf("  throughput     %.0f req/s\n", float64(s.sent.Load())/elapsed.Seconds())
	fmt.Printf("  latency        p50 %s   p95 %s   p99 %s\n",
		p50.Round(time.Millisecond), p95.Round(time.Millisecond), p99.Round(time.Millisecond))
	fmt.Printf("  ok             %d\n", s.ok.Load())
	if *mode == "charge" {
		fmt.Printf("  replayed       %d   (idempotent hits — these did NOT charge again)\n", s.replayed.Load())
		fmt.Printf("  in-flight 409  %d   (concurrent duplicate, correctly refused)\n", s.conflict.Load())
		fmt.Printf("\n  distinct idempotency keys sent: %d\n", *total / *dupes)
		fmt.Printf("  → charges in the database must equal that number, not %d.\n", s.sent.Load())
	}
	fmt.Printf("  rate limited   %d\n", s.rateLtd.Load())
	fmt.Printf("  failed         %d\n", s.failed.Load())
	fmt.Println("──────────────────────────────────────────────────")
}

func doRead(c *http.Client, s *stats, api, key string) {
	doGet(c, s, api+"/v1/dashboard/charges?limit=20", key)
}

func doGet(c *http.Client, s *stats, url, key string) {
	req, _ := http.NewRequestWithContext(context.Background(), "GET", url, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	send(c, s, req)
}

// randomPAN generates a Luhn-valid Visa test number.
//
// Necessary, not cosmetic. Driving a million charges through ONE card number
// is card testing, and the risk engine correctly treats it as such — a first
// attempt at this run was declined 16,906 times out of 17,020 with
// velocity_card_hourly, velocity_card_daily and card_testing_declines firing.
// The load generator was the attack the fraud rules exist to stop.
//
// Real merchant traffic is many cards from many addresses, so that is what has
// to be generated to measure throughput rather than to re-prove the rules work.
func randomPAN(rng *rand.Rand) string {
	digits := make([]int, 16)
	digits[0] = 4 // Visa
	for i := 1; i < 15; i++ {
		digits[i] = rng.Intn(10)
	}
	// Luhn check digit: double every second digit from the right, subtract 9
	// from anything over 9, and choose the final digit to make the sum ≡ 0 mod 10.
	sum := 0
	for i := 14; i >= 0; i-- {
		d := digits[i]
		if (15-i)%2 == 1 {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	digits[15] = (10 - sum%10) % 10

	out := make([]byte, 16)
	for i, d := range digits {
		out[i] = byte('0' + d)
	}
	return string(out)
}

func doCharge(c *http.Client, s *stats, api, vaultURL, key string, keyIndex int) {
	// A token per attempt: they are single-use, so a duplicate request needs its
	// own. The charge is still deduplicated by the idempotency key, which is
	// exactly the property under test — the SECOND request carries a valid but
	// different token and must still not produce a second charge.
	// A distinct card and a distinct source address per charge, because that is
	// what a merchant's real traffic looks like. See randomPAN.
	rng := rand.New(rand.NewSource(rand.Int63()))
	ip := fmt.Sprintf("10.%d.%d.%d", rng.Intn(255), rng.Intn(255), rng.Intn(255))

	tok := tokenize(c, vaultURL, randomPAN(rng), ip)
	if tok == "" {
		s.failed.Add(1)
		s.sent.Add(1)
		return
	}

	// Vary the amount too. A constant amount makes amt_zscore_card meaningless
	// and would not exercise the ledger with realistic values.
	amount := 500 + rng.Intn(40_000)
	body := fmt.Sprintf(`{"amount":%d,"currency":"USD","payment_token":%q}`, amount, tok)
	req, _ := http.NewRequestWithContext(context.Background(), "POST",
		api+"/v1/charges", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	// runID keeps keys unique across invocations. Without it a second run
	// reuses load-0, load-1, ... from the first with different bodies, and the
	// API correctly answers 422 idempotency_key_reused to every single request
	// — which looks exactly like a total outage until you read the status code.
	req.Header.Set("Idempotency-Key", fmt.Sprintf("%s-%d", runID, keyIndex))
	// The charge must carry the same source address as its tokenization, or the
	// per-IP velocity rule sees every charge in the run arriving from 127.0.0.1
	// and declines them all.
	req.Header.Set("X-Forwarded-For", ip)
	send(c, s, req)
}

func tokenize(c *http.Client, vaultURL, pan, ip string) string {
	req, _ := http.NewRequest("POST", vaultURL+"/vault/tokenize", bytes.NewReader(
		[]byte(fmt.Sprintf(`{"number":%q,"exp_month":12,"exp_year":2030}`, pan))))
	req.Header.Set("Content-Type", "application/json")
	// The vault rate limits per IP, and a load test is one IP. Spread the
	// synthetic source addresses so the generator is not measuring its own 429s
	// instead of the server.
	req.Header.Set("X-Forwarded-For", ip)

	res, err := c.Do(req)
	if err != nil {
		return ""
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	// Deliberately crude rather than pulling in a JSON decode per request.
	const marker = `"token":"`
	i := bytes.Index(raw, []byte(marker))
	if i < 0 {
		return ""
	}
	rest := raw[i+len(marker):]
	j := bytes.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return string(rest[:j])
}

func send(c *http.Client, s *stats, req *http.Request) {
	start := time.Now()
	res, err := c.Do(req)
	s.sent.Add(1)
	if err != nil {
		s.failed.Add(1)
		return
	}
	// Drain and close, or the connection cannot be reused and the pool is
	// pointless.
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	s.record(time.Since(start))

	switch {
	case res.Header.Get("Idempotent-Replay") == "true":
		s.replayed.Add(1)
		s.ok.Add(1)
	case res.StatusCode == 429:
		s.rateLtd.Add(1)
	case res.StatusCode == 409:
		s.conflict.Add(1)
	case res.StatusCode >= 200 && res.StatusCode < 300:
		s.ok.Add(1)
	default:
		s.failed.Add(1)
	}
}
