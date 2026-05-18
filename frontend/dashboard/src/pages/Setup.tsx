import { useState, type ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { auth } from '../lib/api';
import { Card, Loading } from '../components/ui';

/**
 * The integration guide.
 *
 * It lives inside the dashboard rather than in a docs site because the useful
 * version is the one filled in with the reader's own values. Everything here is
 * copy-pasteable as-is.
 *
 * The two paths are presented as a genuine choice, not a funnel: a payment link
 * needs no code at all, and most people asking "how do I take a payment" should
 * take that one.
 */

const VAULT_URL = import.meta.env.VITE_VAULT_URL ?? 'http://localhost:8081';
const API_URL = import.meta.env.VITE_API_URL ?? 'http://localhost:8080';

function Code({ children, label }: { children: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="relative">
      {label && (
        <div className="border-b border-slate-800 px-4 py-1.5 text-[11px] font-medium uppercase tracking-wide text-slate-400">
          {label}
        </div>
      )}
      <button
        onClick={() => {
          navigator.clipboard.writeText(children);
          setCopied(true);
          setTimeout(() => setCopied(false), 1500);
        }}
        className="absolute right-2 top-2 rounded border border-slate-700 bg-slate-800 px-2 py-0.5 text-[11px] text-slate-300 hover:bg-slate-700"
      >
        {copied ? 'Copied' : 'Copy'}
      </button>
      <pre className="overflow-x-auto px-4 py-3 text-xs leading-relaxed text-slate-100">
        <code>{children}</code>
      </pre>
    </div>
  );
}

function Block({ children, label }: { children: string; label?: string }) {
  return (
    <div className="overflow-hidden rounded-lg bg-slate-900">
      <Code label={label}>{children}</Code>
    </div>
  );
}

function Step({ n, title, children }: { n: number; title: string; children: ReactNode }) {
  return (
    <div className="flex gap-4">
      <div className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-slate-900 text-xs font-semibold text-white">
        {n}
      </div>
      <div className="min-w-0 flex-1 space-y-3 pb-8">
        <h3 className="text-sm font-semibold text-slate-900">{title}</h3>
        {children}
      </div>
    </div>
  );
}

function P({ children }: { children: ReactNode }) {
  return <p className="text-sm leading-relaxed text-slate-600">{children}</p>;
}

export function Setup() {
  const [tab, setTab] = useState<'link' | 'api'>('link');
  const { data: user, isLoading } = useQuery({ queryKey: ['me'], queryFn: auth.me });

  if (isLoading || !user) return <Loading />;

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-xl font-semibold text-slate-900">Set up PayFlow</h1>
        <p className="mt-1 text-sm text-slate-500">
          Two ways to take a payment. Pick the one that matches what you have.
        </p>
      </div>

      <div className="flex gap-1 rounded-lg bg-slate-100 p-1">
        {(
          [
            ['link', 'No code', 'A URL you send to a customer'],
            ['api', 'API', 'You have a website and a server'],
          ] as const
        ).map(([value, label, hint]) => (
          <button
            key={value}
            onClick={() => setTab(value)}
            className={`flex-1 rounded-md px-4 py-2 text-left transition ${
              tab === value ? 'bg-white shadow-sm' : 'hover:bg-white/50'
            }`}
          >
            <span className="block text-sm font-medium text-slate-900">{label}</span>
            <span className="block text-xs text-slate-500">{hint}</span>
          </button>
        ))}
      </div>

      {tab === 'link' ? (
        <Card title="Payment links">
          <div className="space-y-1 p-5">
            <Step n={1} title="Create a link">
              <P>
                Go to <strong>Payment links</strong>, enter what you are selling
                and the price. You get a URL.
              </P>
            </Step>
            <Step n={2} title="Send it to your customer">
              <P>
                Email, WhatsApp, a QR code on a printed invoice — anywhere a link
                works. They open it, type their card, and pay.
              </P>
            </Step>
            <Step n={3} title="Watch it land">
              <P>
                The charge appears under <strong>Charges</strong> the moment it
                completes, with the same fraud screening, ledger entries and
                webhooks as any other payment.
              </P>
            </Step>

            <div className="ml-11 rounded-lg border border-slate-200 bg-slate-50 p-4">
              <h4 className="text-xs font-semibold uppercase tracking-wide text-slate-500">
                What you are trusting us with
              </h4>
              <p className="mt-2 text-sm leading-relaxed text-slate-600">
                The payment page is served by PayFlow, so we hold the price and
                create the charge on your behalf. That is the trade: you write no
                code and hold no key, and in exchange the checkout is our page,
                not yours. If you need it on your own domain, use the API below.
              </p>
            </div>
          </div>
        </Card>
      ) : (
        <Card title="API integration">
          <div className="space-y-1 p-5">
            <Step n={1} title="Collect the card in our iframe">
              <P>
                Add two lines to your checkout page. The card fields are served
                from <code className="rounded bg-slate-100 px-1 text-xs">{VAULT_URL}</code>,
                a different origin from your site — which means your own
                JavaScript <em>cannot read the card number</em>. That browser
                boundary is what keeps your servers out of PCI scope, and it only
                works as long as you do not build your own card fields.
              </P>
              <Block label="your checkout page">
                {`<div id="card"></div>
<button id="pay">Pay</button>

<script src="${VAULT_URL}/checkout/payflow.js"></script>
<script>
  const payflow = PayFlow('${user.publishable_key}', {
    vaultUrl: '${VAULT_URL}',
  });
  const card = payflow.createCardElement('#card');

  document.getElementById('pay').onclick = async () => {
    const { token, error } = await card.tokenize();
    if (error) return alert(error.message);

    // Send the TOKEN to your server. Never the card.
    await fetch('/checkout', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ payment_token: token }),
    });
  };
</script>`}
              </Block>
            </Step>

            <Step n={2} title="Charge it from your server">
              <P>
                Your server holds the secret key and exchanges the token for a
                charge. The <code className="rounded bg-slate-100 px-1 text-xs">Idempotency-Key</code>{' '}
                is required, and it must identify the <strong>order</strong> —
                not the attempt. A fresh UUID per click is the most common
                integration mistake there is, and it is the one that
                double-charges people.
              </P>
              <Block label="your server">
                {`curl -X POST ${API_URL}/v1/charges \\
  -H "Authorization: Bearer $PAYFLOW_SECRET_KEY" \\
  -H "Idempotency-Key: order_1234" \\
  -H "Content-Type: application/json" \\
  -d '{
    "amount": 4500,
    "currency": "USD",
    "payment_token": "tok_...",
    "metadata": { "order_id": "order_1234" }
  }'`}
              </Block>
              <P>
                Amounts are integers in minor units — 4500 is $45.00. There are no
                decimals anywhere in this API, deliberately.
              </P>
            </Step>

            <Step n={3} title="Handle the three outcomes">
              <P>
                Not two. A <code className="rounded bg-slate-100 px-1 text-xs">202</code> means we
                could not confirm the result — the money may have moved. Do not
                ship the goods, and do not retry with a new key. Reconciliation
                resolves it and a webhook follows.
              </P>
              <Block>
                {`200  status "succeeded"                 → fulfil the order
202  status "requires_reconciliation"   → wait. Do NOT retry.
4xx  card_error / invalid_request       → show the message, let them retry`}
              </Block>
            </Step>

            <Step n={4} title="Verify webhooks before you trust them">
              <P>
                Anyone can POST to a public URL. Check the signature{' '}
                <em>before</em> parsing the body, and deduplicate on the event id
                — delivery is at-least-once, so a retry will arrive.
              </P>
              <Block label="node">
                {`const crypto = require('crypto');

function verify(rawBody, header, secret) {
  const [t, sig] = header.split(',').map(p => p.split('=')[1]);
  const expected = crypto
    .createHmac('sha256', secret)
    .update(\`\${t}.\${rawBody}\`)
    .digest('hex');

  // Constant-time: a plain === leaks the signature one byte at a time.
  return crypto.timingSafeEqual(Buffer.from(sig), Buffer.from(expected))
    && Math.abs(Date.now() / 1000 - Number(t)) < 300;
}`}
              </Block>
            </Step>

            <Step n={5} title="Test the paths that are hard to reach">
              <P>
                Test-mode keys accept an{' '}
                <code className="rounded bg-slate-100 px-1 text-xs">X-Simulate-Outcome</code>{' '}
                header so you can exercise a decline and a timeout on purpose.
                Live keys ignore it.
              </P>
              <Block>
                {`4242 4242 4242 4242      succeeds
X-Simulate-Outcome: decline   card declined
X-Simulate-Outcome: timeout   the ambiguous 202 path`}
              </Block>
            </Step>
          </div>
        </Card>
      )}

      <Card title="Your details">
        <dl className="divide-y divide-slate-100 text-sm">
          {[
            ['Business', user.business_name],
            ['Publishable key', user.publishable_key],
            ['Merchant ID', user.merchant_id],
            ['Vault origin', VAULT_URL],
            ['API base URL', API_URL],
          ].map(([k, v]) => (
            <div key={k} className="flex items-center justify-between gap-4 px-5 py-3">
              <dt className="text-slate-500">{k}</dt>
              <dd className="truncate font-mono text-xs text-slate-800">{v}</dd>
            </div>
          ))}
        </dl>
        <p className="border-t border-slate-100 px-5 py-3 text-xs text-slate-500">
          The publishable key is public — it appears in your page source by
          design and authorises nothing on its own. Your secret key is on the API
          keys page, and is shown only once.
        </p>
      </Card>
    </div>
  );
}
