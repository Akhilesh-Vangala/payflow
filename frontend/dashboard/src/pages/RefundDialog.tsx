import { useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { api, formatMoney, type Charge } from '../lib/api';

/**
 * Refund a charge, fully or partially.
 *
 * Three things here are not incidental.
 *
 * 1. The idempotency key is generated ONCE, when the dialog opens, and reused
 *    for every attempt from that dialog. One dialog is one decision to refund.
 *    A key generated per click would make a double-click two refunds, and a key
 *    derived from the amount would silently block a merchant who legitimately
 *    wants to refund $10 twice.
 *
 * 2. The remaining refundable amount is computed from the charge, and the input
 *    is clamped to it. The server enforces this too — `charge_refund_totals`
 *    caps the total under a row lock — so this is a courtesy, not the control.
 *
 * 3. `requires_reconciliation` is shown as its own outcome, neither success nor
 *    failure. The refund may have reached the processor; telling the merchant
 *    it failed would invite them to refund again.
 */
export function RefundDialog({ charge, onClose }: { charge: Charge; onClose: () => void }) {
  const qc = useQueryClient();
  const remaining = charge.amount - charge.amount_refunded;

  // Generated once per dialog, deliberately. See note 1 above.
  const idempotencyKey = useRef(
    `refund_${charge.id}_${crypto.randomUUID()}`
  ).current;

  const [full, setFull] = useState(true);
  const [amount, setAmount] = useState((remaining / 100).toFixed(2));

  const cents = Math.round(parseFloat(amount || '0') * 100);
  const valid = full || (Number.isFinite(cents) && cents > 0 && cents <= remaining);

  const mutation = useMutation({
    mutationFn: () => api.refund(charge.id, idempotencyKey, full ? undefined : cents),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['charges'] });
      qc.invalidateQueries({ queryKey: ['summary'] });
      qc.invalidateQueries({ queryKey: ['balances'] });
    },
  });

  const result = mutation.data;

  return (
    <div className="fixed inset-0 z-20 flex items-center justify-center bg-slate-900/40 px-4">
      <div className="w-full max-w-md rounded-xl border border-slate-200 bg-white p-6 shadow-xl">
        {result ? (
          <>
            <h2 className="text-sm font-semibold text-slate-900">
              {result.status === 'succeeded'
                ? 'Refund issued'
                : result.status === 'requires_reconciliation'
                  ? 'Refund is being confirmed'
                  : 'Refund failed'}
            </h2>
            <p className="mt-2 text-sm text-slate-600">
              {result.status === 'succeeded' && (
                <>
                  {formatMoney(result.amount, result.currency)} is on its way back to the
                  cardholder. It usually appears on their statement in 5–10 days.
                </>
              )}
              {result.status === 'requires_reconciliation' && (
                <>
                  We could not confirm the outcome with the processor. The money may
                  already have moved, so <strong>do not refund again</strong> — this
                  resolves automatically and you will get a webhook.
                </>
              )}
              {result.status === 'failed' && (
                <>The refund did not go through. {result.failure_code}</>
              )}
            </p>
            <button
              onClick={onClose}
              className="mt-5 w-full rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-800"
            >
              Done
            </button>
          </>
        ) : (
          <>
            <h2 className="text-sm font-semibold text-slate-900">Refund payment</h2>
            <p className="mt-1 text-xs text-slate-500">
              {formatMoney(charge.amount, charge.currency)} charged
              {charge.amount_refunded > 0 && (
                <> · {formatMoney(charge.amount_refunded, charge.currency)} already refunded</>
              )}
            </p>

            <div className="mt-5 space-y-2">
              <label className="flex items-start gap-2 text-sm">
                <input type="radio" checked={full} onChange={() => setFull(true)} className="mt-1" />
                <span>
                  <span className="font-medium text-slate-800">Full refund</span>
                  <span className="block text-xs text-slate-500">
                    {formatMoney(remaining, charge.currency)} — everything still refundable
                  </span>
                </span>
              </label>
              <label className="flex items-start gap-2 text-sm">
                <input
                  type="radio"
                  checked={!full}
                  onChange={() => setFull(false)}
                  className="mt-1"
                />
                <span className="flex-1">
                  <span className="font-medium text-slate-800">Partial refund</span>
                  <input
                    value={amount}
                    inputMode="decimal"
                    disabled={full}
                    onChange={(e) => setAmount(e.target.value)}
                    className="mt-1 w-full rounded-lg border border-slate-300 px-3 py-1.5 text-sm tabular-nums disabled:bg-slate-50 disabled:text-slate-400 focus:border-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-500/20"
                  />
                  {!full && cents > remaining && (
                    <span className="mt-1 block text-xs text-red-600">
                      Cannot exceed {formatMoney(remaining, charge.currency)}.
                    </span>
                  )}
                </span>
              </label>
            </div>

            <p className="mt-4 rounded-lg bg-slate-50 p-3 text-xs text-slate-600">
              Refunds go back to the original card and cannot be reversed. To take the
              money again you would have to create a new charge.
            </p>

            {mutation.isError && (
              <p className="mt-3 text-xs text-red-600">{(mutation.error as Error).message}</p>
            )}

            <div className="mt-5 flex gap-2">
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
                {mutation.isPending
                  ? 'Refunding…'
                  : `Refund ${formatMoney(full ? remaining : cents || 0, charge.currency)}`}
              </button>
            </div>
          </>
        )}
      </div>
    </div>
  );
}
