import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, formatDate, formatMoney, type PaymentLink } from '../lib/api';
import { Card, Empty, Failed, Loading, Table } from '../components/ui';

/**
 * Payment links.
 *
 * The point of this page is that a merchant can take a card payment without
 * writing any code or running any server. The link opens a page PayFlow hosts;
 * the card goes into PayFlow's vault iframe; PayFlow creates the charge from
 * the price stored on the link row.
 *
 * The price is never sent by the browser that pays. That is the single reason
 * a public, unauthenticated payment page is safe to expose at all.
 */

function NewLinkDialog({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState('');
  const [amount, setAmount] = useState('');
  const [description, setDescription] = useState('');
  const [successUrl, setSuccessUrl] = useState('');

  const mutation = useMutation({
    mutationFn: () =>
      api.createLink({
        // Parsed to minor units here, at the edge, and sent as an integer.
        // Everything past this line treats money as int64 — the one place a
        // float is allowed near an amount is a text field a human typed into.
        amount: Math.round(parseFloat(amount || '0') * 100),
        currency: 'USD',
        name,
        description: description || undefined,
        success_url: successUrl || undefined,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['links'] });
      onClose();
    },
  });

  const cents = Math.round(parseFloat(amount || '0') * 100);
  const valid = name.trim().length > 0 && Number.isFinite(cents) && cents > 0;

  return (
    <div className="fixed inset-0 z-20 flex items-center justify-center bg-slate-900/40 px-4">
      <div className="w-full max-w-lg rounded-xl border border-slate-200 bg-white p-6 shadow-xl">
        <h2 className="text-sm font-semibold text-slate-900">New payment link</h2>
        <p className="mt-1 text-xs text-slate-500">
          You will get a URL you can send to anyone. No code, no server.
        </p>

        <div className="mt-5 space-y-4">
          <label className="block">
            <span className="text-xs font-medium text-slate-700">What is it for</span>
            <input
              value={name}
              autoFocus
              onChange={(e) => setName(e.target.value)}
              placeholder="Consultation — 1 hour"
              className="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-500/20"
            />
          </label>

          <label className="block">
            <span className="text-xs font-medium text-slate-700">Amount (USD)</span>
            <input
              value={amount}
              inputMode="decimal"
              onChange={(e) => setAmount(e.target.value)}
              placeholder="45.00"
              className="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm tabular-nums focus:border-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-500/20"
            />
            {cents > 0 && (
              <span className="mt-1 block text-xs text-slate-400">
                Charged as {cents} cents.
              </span>
            )}
          </label>

          <label className="block">
            <span className="text-xs font-medium text-slate-700">Description</span>
            <input
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="Optional — shown on the payment page"
              className="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-500/20"
            />
          </label>

          <label className="block">
            <span className="text-xs font-medium text-slate-700">Redirect after payment</span>
            <input
              value={successUrl}
              onChange={(e) => setSuccessUrl(e.target.value)}
              placeholder="https://example.com/thanks — optional"
              className="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-500/20"
            />
            <span className="mt-1 block text-xs text-slate-400">
              Leave blank to show a PayFlow receipt instead.
            </span>
          </label>
        </div>

        {mutation.isError && (
          <p className="mt-3 text-xs text-red-600">{(mutation.error as Error).message}</p>
        )}

        <div className="mt-6 flex gap-2">
          <button
            onClick={onClose}
            className="flex-1 rounded-lg border border-slate-300 px-4 py-2 text-sm font-medium text-slate-700 hover:bg-slate-50"
          >
            Cancel
          </button>
          <button
            onClick={() => mutation.mutate()}
            disabled={!valid || mutation.isPending}
            className="flex-1 rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-800 disabled:opacity-40"
          >
            {mutation.isPending ? 'Creating…' : 'Create link'}
          </button>
        </div>
      </div>
    </div>
  );
}

function Row({ link }: { link: PaymentLink }) {
  const qc = useQueryClient();
  const [copied, setCopied] = useState(false);
  const toggle = useMutation({
    mutationFn: () => api.setLinkActive(link.id, !link.active),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['links'] }),
  });

  return (
    <tr className={link.active ? undefined : 'opacity-50'}>
      <td className="px-5 py-3">
        <div className="text-slate-800">{link.name}</div>
        <div className="text-xs text-slate-400">{formatDate(link.created_at)}</div>
      </td>
      <td className="px-5 py-3 tabular-nums text-slate-800">
        {formatMoney(link.amount, link.currency)}
      </td>
      <td className="px-5 py-3">
        <div className="flex items-center gap-2">
          <a
            href={link.url}
            target="_blank"
            rel="noreferrer"
            className="max-w-[16rem] truncate font-mono text-xs text-indigo-600 hover:text-indigo-500"
          >
            {link.url}
          </a>
          <button
            onClick={() => {
              navigator.clipboard.writeText(link.url);
              setCopied(true);
              setTimeout(() => setCopied(false), 1500);
            }}
            className="shrink-0 rounded border border-slate-300 px-1.5 py-0.5 text-[11px] text-slate-600 hover:bg-slate-50"
          >
            {copied ? 'Copied' : 'Copy'}
          </button>
        </div>
      </td>
      <td className="px-5 py-3 tabular-nums text-slate-700">
        {link.payments}
        {link.payments > 0 && (
          <span className="ml-2 text-xs text-slate-400">
            {formatMoney(link.volume, link.currency)}
          </span>
        )}
      </td>
      <td className="px-5 py-3 text-right">
        <button
          onClick={() => toggle.mutate()}
          className="text-xs font-medium text-slate-600 hover:text-slate-900"
        >
          {link.active ? 'Deactivate' : 'Reactivate'}
        </button>
      </td>
    </tr>
  );
}

export function Links() {
  const [creating, setCreating] = useState(false);
  const { data, isLoading, error } = useQuery({ queryKey: ['links'], queryFn: api.links });

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-xl font-semibold text-slate-900">Payment links</h1>
        <p className="mt-1 text-sm text-slate-500">
          A URL that takes a card payment. Send it in an email, a message, or a
          QR code — the customer pays on a page we host, and the charge appears
          in your dashboard like any other.
        </p>
      </div>

      <Card
        title="Links"
        action={
          <button
            onClick={() => setCreating(true)}
            className="rounded-md bg-slate-900 px-3 py-1.5 text-xs font-medium text-white hover:bg-slate-800"
          >
            New link
          </button>
        }
      >
        {isLoading ? (
          <Loading />
        ) : error ? (
          <Failed error={error} />
        ) : !data?.data.length ? (
          <Empty message="No payment links yet. Create one to take a payment without writing any code." />
        ) : (
          <Table headers={['Name', 'Amount', 'Link', 'Paid', '']}>
            {data.data.map((l) => (
              <Row key={l.id} link={l} />
            ))}
          </Table>
        )}
      </Card>

      <p className="text-xs text-slate-500">
        Deactivating a link stops new payments. It is never deleted — a link that
        has taken money is part of the audit trail for those charges.
      </p>

      {creating && <NewLinkDialog onClose={() => setCreating(false)} />}
    </div>
  );
}
