# Integrating PayFlow

**Nothing in the PayFlow codebase changes to onboard a merchant.** A merchant
signs up, gets an API key, and writes the two pieces below in whatever language
they already use. This directory is an illustration of those two pieces, not a
service PayFlow runs.

---

## The three parties, and why it is three

```
  BROWSER                    MERCHANT SERVER              PAYFLOW
  ───────                    ───────────────              ───────
  card number ──────────────────────────────────────────▶ vault
                                                          (iframe on
   ◀──────────────────────────────────────────────────── PayFlow's origin)
   token

   token ─────────────────▶  has the secret key
                             ────────────────────────────▶ POST /v1/charges
                             ◀──────────────────────────── charge
   order status ◀─────────

                             ◀──────────────────────────── signed webhook
```

No two parties hold both halves:

- The **browser** has the card but no key. It cannot charge anyone.
- The **merchant server** has the key but never the card. There is nothing on
  it to steal, and nothing for a PCI assessor to audit.
- **PayFlow** has both, in one small isolated service, which is the only place
  that needs the scrutiny.

That split is the entire reason integrating PayFlow keeps a merchant in SAQ A
scope instead of full PCI DSS.

---

## What a merchant writes

### 1. A checkout page

Drop in the script, mount the element, hand the token to your own server.

```html
<script src="https://js.payflow.com/v1/payflow.js"></script>
<div id="card"></div>
<button id="pay">Pay</button>

<script>
  const payflow = PayFlow('pk_test_...');
  const card = payflow.createCardElement('#card');

  document.querySelector('#pay').onclick = async () => {
    const { token, error } = await card.tokenize();
    if (error) return showError(error.message);

    // Your server. Never call PayFlow directly from the browser — that
    // would require putting your secret key in client-side code.
    await fetch('/checkout', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ payment_token: token }),
    });
  };
</script>
```

`payflow.js` is vanilla JavaScript with no dependencies, so it works with
React, Vue, Angular, WordPress or hand-written HTML alike.

### 2. A server endpoint

Three requirements, in order of how expensive it is to get them wrong:

**Derive the idempotency key from the order, not per attempt.**

```
Idempotency-Key: order_1a2b3c
```

If the customer double-clicks Pay, or their connection drops and the browser
retries, both requests carry the same key and PayFlow charges once. Generating
a fresh UUID per attempt defeats the guarantee entirely — it is the single
most common integration mistake.

**Treat 202 as unresolved, not as failure.** A `202` means PayFlow could not
confirm the outcome with the bank. The money may have moved. Do not ship the
goods, and do not tell the customer it failed; reconciliation resolves it
within the hour and a webhook follows.

**Never put your secret key in the browser.** `sk_` keys belong on your server
only. `payflow.js` refuses a secret key outright, but that is a safety net,
not a design.

```
POST https://api.payflow.com/v1/charges
Authorization: Bearer sk_test_...
Idempotency-Key: order_1a2b3c
Content-Type: application/json

{ "amount": 9500, "currency": "USD", "payment_token": "tok_..." }
```

Amounts are integers in the currency's minor unit. `9500` is $95.00. Never
send a decimal.

---

## Webhooks

Payments settle asynchronously and disputes arrive days later, so the charge
response is not the last word. Register an endpoint and handle events.

**Delivery is at-least-once.** A retry will arrive. Deduplicate on the event
`id` — that guarantee is why the id exists.

**Verify the signature before trusting anything.** Anyone can POST to a public
URL. Without this check, a stranger can tell you a payment succeeded.

```
PayFlow-Signature: t=1788393751,v1=5257a8...
```

The signed material is `{timestamp}.{raw body}`, HMAC-SHA256 with your webhook
secret. The timestamp is inside the signature so a captured webhook cannot be
replayed forever — reject anything older than about five minutes.

```python
# Python, ~10 lines. Same shape in any language.
import hmac, hashlib, time

def verify(body: bytes, header: str, secret: str, tolerance=300) -> bool:
    parts = dict(p.split('=', 1) for p in header.split(','))
    ts = int(parts['t'])
    if abs(time.time() - ts) > tolerance:
        return False                       # too old — possible replay
    expected = hmac.new(
        secret.encode(), f'{ts}.'.encode() + body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, parts['v1'])   # constant-time
```

Use a constant-time comparison. A plain `==` leaks how much of the signature
was correct, which is enough to forge one given enough attempts.

**Return 200 even for a duplicate.** Returning an error makes PayFlow retry an
event you have already handled.

**During secret rotation both secrets sign**, so a `PayFlow-Signature` header
may carry two `v1` values. Accept the payload if *either* verifies — otherwise
rotating breaks every delivery in flight.

---

## Running this example

```bash
make up
make seed                       # prints an sk_test_... key
export PAYFLOW_API_KEY=sk_test_...
go run ./examples/merchant-server
```

Open http://localhost:3000, pay with `4242 4242 4242 4242`, and the charge
appears in the PayFlow dashboard at http://localhost:5173.

The example is Go because this repo already has the toolchain. The integration
is three HTTP calls — nothing about it is language-specific.

---

## Test cards

| Number | Behaviour |
|---|---|
| `4242 4242 4242 4242` | Visa, succeeds |
| `5555 5555 5555 4444` | Mastercard, succeeds |
| `3714 496353 98431` | Amex, succeeds |
| `6011 1111 1111 1117` | Discover, succeeds |

In test mode, `X-Simulate-Outcome` on a charge forces a specific result:
`success`, `decline`, `timeout`, `delayed_success`, `network_error`. Live keys
ignore the header — a merchant cannot steer the real processor.

Use `timeout` to check your `202` handling. That path is the one most
integrations get wrong, and the one that costs real money when they do.
