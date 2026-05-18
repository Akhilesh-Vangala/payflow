import { detectNetwork, formatNumber, onlyDigits, validateCard,
         cvcLength, maxLength, NETWORKS } from './card.js';
import { createBehaviorTracker } from './behavior.js';

// How the card is entered is a fraud signal, and this frame is the only place
// in the system that can observe it — the merchant's page cannot reach inside,
// and by the time the charge reaches the API the keystrokes are long gone.
const behavior = createBehaviorTracker();

const els = {
  form: document.getElementById('form'),
  number: document.getElementById('number'),
  expiry: document.getElementById('expiry'),
  cvc: document.getElementById('cvc'),
  badge: document.getElementById('badge'),
};

// The parent origin is supplied by payflow.js in the iframe URL and is the ONLY
// origin we will ever postMessage to. Never '*' — that would broadcast the
// token (and any error detail) to whatever page happens to be listening.
const params = new URLSearchParams(location.search);
const parentOrigin = params.get('origin') || '';
const merchantId = params.get('merchant_id') || '';
const vaultBase = params.get('vault') || '';

let network = NETWORKS.UNKNOWN;

behavior.attach(els.number, 'number');
behavior.attach(els.expiry, 'expiry');
behavior.attach(els.cvc, 'cvc');

function post(type, payload = {}) {
  if (!parentOrigin) return;
  parent.postMessage({ source: 'payflow-checkout', type, ...payload }, parentOrigin);
}

function setError(field, message) {
  document.getElementById(`${field}-error`).textContent = message || '';
  els[field].classList.toggle('invalid', Boolean(message));
}

// --- live formatting and network detection --------------------------------

els.number.addEventListener('input', () => {
  const before = els.number.selectionStart;
  const rawLength = els.number.value.length;

  network = detectNetwork(els.number.value);
  els.number.value = formatNumber(els.number.value, network);

  // Keep the caret sensible when spaces are inserted ahead of it, so typing
  // mid-number doesn't jump the cursor to the end.
  const delta = els.number.value.length - rawLength;
  if (before !== null && before < rawLength) {
    els.number.setSelectionRange(before + delta, before + delta);
  }

  if (network === NETWORKS.UNKNOWN) {
    els.badge.classList.remove('show');
  } else {
    els.badge.textContent = network === NETWORKS.AMEX ? 'AMEX' : network;
    els.badge.dataset.network = network;
    els.badge.classList.add('show');
  }

  els.cvc.maxLength = cvcLength(network);
  setError('number', '');

  // Advance to expiry once the number is complete — the small courtesy that
  // makes a card form feel finished.
  if (onlyDigits(els.number.value).length === maxLength(network)) {
    els.expiry.focus();
  }
  post('change', { network, empty: onlyDigits(els.number.value).length === 0 });
});

els.expiry.addEventListener('input', () => {
  let digits = onlyDigits(els.expiry.value).slice(0, 4);
  // A leading digit above 1 can only be a single-digit month: 4 → 04.
  if (digits.length === 1 && digits > '1') digits = '0' + digits;
  els.expiry.value = digits.length > 2 ? `${digits.slice(0, 2)} / ${digits.slice(2)}` : digits;
  setError('expiry', '');
  if (digits.length === 4) els.cvc.focus();
});

els.cvc.addEventListener('input', () => {
  els.cvc.value = onlyDigits(els.cvc.value).slice(0, cvcLength(network));
  setError('cvc', '');
});

function parseExpiry() {
  const digits = onlyDigits(els.expiry.value);
  return {
    month: parseInt(digits.slice(0, 2), 10) || 0,
    year: digits.length >= 4 ? 2000 + parseInt(digits.slice(2, 4), 10) : 0,
  };
}

// --- tokenization ----------------------------------------------------------

async function tokenize() {
  const { month, year } = parseExpiry();
  const card = {
    number: onlyDigits(els.number.value),
    expMonth: month,
    expYear: year,
    cvc: els.cvc.value,
  };

  const { valid, errors } = validateCard(card);
  setError('number', errors.number);
  setError('expiry', errors.expiry);
  setError('cvc', errors.cvc);

  if (!valid) {
    post('error', { code: 'validation_failed', message: 'Please check the card details.' });
    return;
  }

  try {
    const resp = await fetch(`${vaultBase}/vault/tokenize`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        number: card.number,
        exp_month: card.expMonth,
        exp_year: card.expYear,
        merchant_id: merchantId || undefined,
        single_use: true,
        // Aggregates only. Sent to the vault rather than handed to the parent
        // page, so the merchant cannot forge or suppress them.
        behavior: behavior.summarize(),
      }),
    });

    const body = await resp.json();

    if (!resp.ok) {
      const message = body?.error?.message || 'The card could not be processed.';
      if (body?.error?.param === 'number') setError('number', message);
      post('error', { code: body?.error?.code || 'tokenization_failed', message });
      return;
    }

    // Only the token crosses the frame boundary — never the card number.
    // The CVC is deliberately not tokenized or stored: PCI prohibits storing
    // it, so it is passed to the processor at charge time and nowhere else.
    post('token', {
      token: body.token,
      brand: body.brand,
      last4: body.last4,
      expMonth: body.exp_month,
      expYear: body.exp_year,
    });

    // Clear the fields once tokenized so the PAN does not linger in the DOM.
    els.form.reset();
    els.badge.classList.remove('show');
  } catch (err) {
    // The caught error may reference the request; never forward its detail.
    post('error', { code: 'network_error', message: 'Could not reach PayFlow. Please try again.' });
  }
}

// --- parent messaging ------------------------------------------------------

window.addEventListener('message', (event) => {
  // Reject anything not from the exact parent origin we were configured with.
  // Without this check, any page could drive this frame.
  if (event.origin !== parentOrigin) return;
  if (event.data?.source !== 'payflow-parent') return;

  switch (event.data.type) {
    case 'tokenize':
      tokenize();
      break;
    case 'focus':
      els.number.focus();
      break;
    case 'clear':
      els.form.reset();
      els.badge.classList.remove('show');
      break;
  }
});

// Report height so the parent can size the iframe to its content.
const reportHeight = () => post('resize', { height: document.body.scrollHeight });
new ResizeObserver(reportHeight).observe(document.body);

post('ready');
