import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, formatDate, type ApiKey } from '../lib/api';
import { Badge, Card, Empty, Failed, Loading, Table } from '../components/ui';

/**
 * API keys.
 *
 * The list can never show a key. Only `key_prefix` is stored — the rest exists
 * as a SHA-256 digest and nothing else — so a leaked database, a compromised
 * dashboard session and a curious employee all get the same thing from this
 * page: the first sixteen characters.
 */

function NewKeyDialog({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState('');
  const [scope, setScope] = useState<'read' | 'write'>('write');
  const [created, setCreated] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);

  const mutation = useMutation({
    mutationFn: () => api.createKey(name, scope),
    onSuccess: (res) => {
      setCreated(res.key);
      qc.invalidateQueries({ queryKey: ['keys'] });
    },
  });

  return (
    <div className="fixed inset-0 z-20 flex items-center justify-center bg-slate-900/40 px-4">
      <div className="w-full max-w-lg rounded-xl border border-slate-200 bg-white p-6 shadow-xl">
        {created ? (
          <>
            <h2 className="text-sm font-semibold text-slate-900">Key created</h2>
            <p className="mt-1 text-sm text-slate-500">
              This is the only time it will be shown.
            </p>
            <div className="mt-4 flex items-center gap-2 rounded-lg border border-slate-200 bg-slate-50 p-3">
              <code className="flex-1 break-all font-mono text-xs text-slate-800">{created}</code>
              <button
                onClick={() => {
                  navigator.clipboard.writeText(created);
                  setCopied(true);
                }}
                className="shrink-0 rounded-md border border-slate-300 bg-white px-3 py-1.5 text-xs font-medium text-slate-700 hover:bg-slate-50"
              >
                {copied ? 'Copied' : 'Copy'}
              </button>
            </div>
            <button
              onClick={onClose}
              className="mt-5 w-full rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-800"
            >
              Done
            </button>
          </>
        ) : (
          <>
            <h2 className="text-sm font-semibold text-slate-900">Create an API key</h2>
            <label className="mt-4 block">
              <span className="text-xs font-medium text-slate-700">Name</span>
              <input
                value={name}
                autoFocus
                onChange={(e) => setName(e.target.value)}
                placeholder="Production server"
                className="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-500/20"
              />
            </label>
            <fieldset className="mt-4">
              <legend className="text-xs font-medium text-slate-700">Permissions</legend>
              <div className="mt-2 space-y-2">
                {(
                  [
                    ['write', 'Full access', 'Create charges, refunds and disputes.'],
                    ['read', 'Read only', 'Cannot move money. Safe for reporting jobs.'],
                  ] as const
                ).map(([value, label, hint]) => (
                  <label key={value} className="flex items-start gap-2 text-sm">
                    <input
                      type="radio"
                      checked={scope === value}
                      onChange={() => setScope(value)}
                      className="mt-1"
                    />
                    <span>
                      <span className="font-medium text-slate-800">{label}</span>
                      <span className="block text-xs text-slate-500">{hint}</span>
                    </span>
                  </label>
                ))}
              </div>
            </fieldset>

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
                disabled={mutation.isPending || !name.trim()}
                className="flex-1 rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-800 disabled:opacity-40"
              >
                {mutation.isPending ? 'Creating…' : 'Create key'}
              </button>
            </div>
          </>
        )}
      </div>
    </div>
  );
}

function Row({ k }: { k: ApiKey }) {
  const qc = useQueryClient();
  const revoke = useMutation({
    mutationFn: () => api.revokeKey(k.id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['keys'] }),
  });

  const revoked = Boolean(k.revoked_at);
  return (
    <tr className={revoked ? 'opacity-50' : undefined}>
      <td className="px-5 py-3 text-slate-800">{k.name || 'Untitled key'}</td>
      <td className="px-5 py-3">
        <code className="font-mono text-xs text-slate-600">{k.prefix}…</code>
      </td>
      <td className="px-5 py-3">
        <Badge value={k.scope === 'read' ? 'read only' : 'full access'} />
      </td>
      <td className="px-5 py-3 text-xs text-slate-500">
        {k.last_used_at ? formatDate(k.last_used_at) : 'never'}
      </td>
      <td className="px-5 py-3 text-xs text-slate-500">{formatDate(k.created_at)}</td>
      <td className="px-5 py-3 text-right">
        {revoked ? (
          <span className="text-xs text-slate-400">revoked</span>
        ) : (
          <button
            onClick={() => {
              // Revocation takes effect on the next request and cannot be
              // undone, so it is worth one deliberate confirmation.
              if (confirm(`Revoke "${k.name || 'this key'}"? Anything using it stops working immediately.`)) {
                revoke.mutate();
              }
            }}
            className="text-xs font-medium text-red-600 hover:text-red-500"
          >
            Revoke
          </button>
        )}
      </td>
    </tr>
  );
}

export function Keys() {
  const [creating, setCreating] = useState(false);
  const { data, isLoading, error } = useQuery({
    queryKey: ['keys'],
    queryFn: api.keys,
  });

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-xl font-semibold text-slate-900">API keys</h1>
        <p className="mt-1 text-sm text-slate-500">
          Keys authenticate your server. They are shown once at creation and
          stored only as a hash — if you lose one, create a new one and revoke
          the old.
        </p>
      </div>

      <Card
        title="Keys"
        action={
          <button
            onClick={() => setCreating(true)}
            className="rounded-md bg-slate-900 px-3 py-1.5 text-xs font-medium text-white hover:bg-slate-800"
          >
            Create key
          </button>
        }
      >
        {isLoading ? (
          <Loading />
        ) : error ? (
          <Failed error={error} />
        ) : !data?.data.length ? (
          <Empty message="No API keys yet." />
        ) : (
          <Table headers={['Name', 'Key', 'Permissions', 'Last used', 'Created', '']}>
            {data.data.map((k) => (
              <Row key={k.id} k={k} />
            ))}
          </Table>
        )}
      </Card>

      <p className="text-xs text-slate-500">
        Only test keys are available in this deployment. Live keys move real
        money and require a sponsor bank and completed onboarding, neither of
        which exists here — issuing one would misrepresent what the account can
        do.
      </p>

      {creating && <NewKeyDialog onClose={() => setCreating(false)} />}
    </div>
  );
}
