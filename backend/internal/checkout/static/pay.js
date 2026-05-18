/**
 * The hosted payment page's own script.
 *
 * It does three things in order, and the order is the interesting part:
 *
 *   1. start a checkout session   — gives this attempt an identity BEFORE any
 *                                   money moves, and that identity becomes the
 *                                   idempotency key server-side
 *   2. tokenize the card          — happens inside the vault's iframe; this
 *                                   file never touches a card number and could
 *                                   not read one if it tried
 *   3. confirm                    — sends the token and the session id, and
 *                                   NOTHING ELSE. In particular no amount: the
 *                                   server reads the price from the session row
 *                                   it created in step 1
 *
 * Step 1 before step 2 is deliberate. A vault token is single-use, so if the
 * session failed to start after tokenizing we would have burned a token and the
 * customer would have to re-enter their card.
 */
(function () {
  'use strict';

  var script = document.currentScript;
  var CODE = script.dataset.code;
  var VAULT = script.dataset.vault;

  var payBtn = document.getElementById('pay');
  var statusEl = document.getElementById('status');
  var formEl = document.getElementById('form');
  var doneEl = document.getElementById('done');

  // The publishable key is not used as a credential by the vault today; it is
  // passed because payflow.js requires one, and because a merchant reading page
  // source should see the same shape they would use in their own integration.
  var client = PayFlow('pk_test_hosted', { vaultUrl: VAULT });
  var card = client.createCardElement('#mount');

  card.on('ready', function () { setStatus(''); });

  function setStatus(text, kind) {
    statusEl.textContent = text;
    statusEl.className = kind || '';
  }

  function finish(title, body) {
    formEl.hidden = true;
    document.getElementById('done-title').textContent = title;
    document.getElementById('done-body').textContent = body || '';
    doneEl.hidden = false;
  }

  async function post(url, body) {
    var res = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body || {}),
    });
    var data = null;
    try { data = await res.json(); } catch (e) { /* empty or non-JSON body */ }
    return { ok: res.ok, status: res.status, data: data };
  }

  function errorMessage(payload, fallback) {
    return (payload && payload.error && payload.error.message) || fallback;
  }

  payBtn.addEventListener('click', async function () {
    payBtn.disabled = true;
    setStatus('Starting a secure checkout…');

    var session = await post('/pay/' + encodeURIComponent(CODE) + '/session');
    if (!session.ok) {
      payBtn.disabled = false;
      setStatus(errorMessage(session.data, 'This payment link is unavailable.'), 'err');
      return;
    }

    setStatus('Checking your card…');
    var result = await card.tokenize();
    if (result.error) {
      payBtn.disabled = false;
      setStatus(result.error.message, 'err');
      return;
    }

    setStatus('Taking payment…');
    var confirm = await post('/pay/session/' + session.data.id + '/confirm', {
      payment_token: result.token,
    });

    if (!confirm.ok) {
      var code = confirm.data && confirm.data.error && confirm.data.error.code;

      // Not all 409s are alike, and treating them alike would charge someone
      // twice. request_in_flight means a concurrent attempt is CHARGING THE
      // CARD right now; re-enabling the button would start a new session with a
      // new idempotency key against a card already in flight. Leave it disabled
      // and let the running attempt finish.
      if (code === 'request_in_flight') {
        setStatus('Still processing your payment — please wait.', '');
        return;
      }

      payBtn.disabled = false;
      // A used or expired session, by contrast, IS safely retryable: nothing is
      // in flight, and pressing pay gets a clean session.
      if (confirm.status === 409) {
        setStatus('That attempt timed out. Press pay to try again.', 'err');
        return;
      }
      setStatus(errorMessage(confirm.data, 'The payment could not be completed.'), 'err');
      return;
    }

    var payment = confirm.data;
    if (payment.status === 'succeeded') {
      setStatus('');
      if (payment.redirect_to) {
        // The merchant asked for the customer to land on their own page.
        window.location.href = payment.redirect_to;
        return;
      }
      finish('Payment complete', 'Reference ' + payment.charge_id);
      return;
    }

    if (payment.status === 'processing') {
      // Ambiguous, not failed. The customer must NOT be invited to pay again —
      // the money may already have moved.
      finish('Payment received', payment.message);
      return;
    }

    payBtn.disabled = false;
    setStatus(payment.message || 'The payment was declined.', 'err');
  });
})();
