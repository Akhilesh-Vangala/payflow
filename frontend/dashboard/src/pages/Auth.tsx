import { useState } from 'react';
import { ApiError, auth, setSession, type AuthSession } from '../lib/api';

/**
 * Sign in and sign up.
 *
 * One thing to notice in the register flow: the API key comes back exactly
 * once, in the registration response, and this component shows it on a screen
 * the person has to dismiss deliberately. There is no endpoint that can return
 * it again — not because we did not build one, but because a support agent who
 * can read a merchant's key can also spend a merchant's money.
 */

function Field({
  label,
  type = 'text',
  value,
  onChange,
  placeholder,
  hint,
  autoFocus,
}: {
  label: string;
  type?: string;
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  hint?: string;
  autoFocus?: boolean;
}) {
  return (
    <label className="block">
      <span className="text-xs font-medium text-slate-700">{label}</span>
      <input
        type={type}
        value={value}
        autoFocus={autoFocus}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
        className="mt-1 w-full rounded-lg border border-slate-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none focus:ring-2 focus:ring-indigo-500/20"
      />
      {hint && <span className="mt-1 block text-xs text-slate-400">{hint}</span>}
    </label>
  );
}

function KeyReveal({ apiKey, onDone }: { apiKey: string; onDone: () => void }) {
  const [copied, setCopied] = useState(false);
  const [ack, setAck] = useState(false);

  return (
    <div className="flex min-h-screen items-center justify-center bg-slate-50 px-4">
      <div className="w-full max-w-lg rounded-xl border border-slate-200 bg-white p-7 shadow-sm">
        <h1 className="text-lg font-semibold text-slate-900">Your test API key</h1>
        <p className="mt-1 text-sm text-slate-500">
          Copy this now. Only a hash of it is stored, so this is the only time it
          can be shown — including to us.
        </p>

        <div className="mt-5 flex items-center gap-2 rounded-lg border border-slate-200 bg-slate-50 p-3">
          <code className="flex-1 break-all font-mono text-xs text-slate-800">{apiKey}</code>
          <button
            onClick={() => {
              navigator.clipboard.writeText(apiKey);
              setCopied(true);
            }}
            className="shrink-0 rounded-md border border-slate-300 bg-white px-3 py-1.5 text-xs font-medium text-slate-700 hover:bg-slate-50"
          >
            {copied ? 'Copied' : 'Copy'}
          </button>
        </div>

        <p className="mt-4 rounded-lg bg-amber-50 p-3 text-xs text-amber-900">
          This key charges cards. Keep it on your server, in a secret manager —
          never in client-side code, a git repository, or a browser.
        </p>

        <label className="mt-5 flex items-start gap-2 text-sm text-slate-700">
          <input
            type="checkbox"
            checked={ack}
            onChange={(e) => setAck(e.target.checked)}
            className="mt-0.5"
          />
          I have saved this key somewhere safe.
        </label>

        <button
          disabled={!ack}
          onClick={onDone}
          className="mt-5 w-full rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-800 disabled:opacity-40"
        >
          Continue to dashboard
        </button>
      </div>
    </div>
  );
}

export function Auth({ onSignedIn }: { onSignedIn: (user: AuthSession['user']) => void }) {
  const [mode, setMode] = useState<'login' | 'register'>('login');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [revealed, setRevealed] = useState<{ key: string; user: AuthSession['user'] } | null>(null);

  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [business, setBusiness] = useState('');
  const [name, setName] = useState('');

  if (revealed) {
    return <KeyReveal apiKey={revealed.key} onDone={() => onSignedIn(revealed.user)} />;
  }

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError('');
    try {
      if (mode === 'login') {
        const session = await auth.login(email, password);
        setSession(session.token);
        onSignedIn(session.user);
      } else {
        const { session, api_key } = await auth.register({
          business_name: business,
          email,
          password,
          name,
        });
        setSession(session.token);
        setRevealed({ key: api_key, user: session.user });
      }
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Something went wrong.');
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-slate-50 px-4 py-10">
      <form onSubmit={submit} className="w-full max-w-sm">
        <div className="rounded-xl border border-slate-200 bg-white p-7 shadow-sm">
          <h1 className="text-lg font-semibold text-slate-900">
            {mode === 'login' ? 'Sign in to PayFlow' : 'Create your PayFlow account'}
          </h1>
          <p className="mt-1 text-sm text-slate-500">
            {mode === 'login'
              ? 'Your merchant dashboard, keys and payment links.'
              : 'You will get a test API key immediately. No card required.'}
          </p>

          <div className="mt-6 space-y-4">
            {mode === 'register' && (
              <>
                <Field
                  label="Business name"
                  value={business}
                  onChange={setBusiness}
                  placeholder="Acme Coffee"
                  autoFocus
                />
                <Field label="Your name" value={name} onChange={setName} placeholder="Optional" />
              </>
            )}
            <Field
              label="Email"
              type="email"
              value={email}
              onChange={setEmail}
              placeholder="you@example.com"
              autoFocus={mode === 'login'}
            />
            <Field
              label="Password"
              type="password"
              value={password}
              onChange={setPassword}
              hint={mode === 'register' ? 'At least 10 characters. Length beats symbols.' : undefined}
            />
          </div>

          {error && <p className="mt-4 text-xs text-red-600">{error}</p>}

          <button
            type="submit"
            disabled={busy}
            className="mt-6 w-full rounded-lg bg-slate-900 px-4 py-2 text-sm font-medium text-white hover:bg-slate-800 disabled:opacity-50"
          >
            {busy ? 'Working…' : mode === 'login' ? 'Sign in' : 'Create account'}
          </button>
        </div>

        <p className="mt-4 text-center text-sm text-slate-500">
          {mode === 'login' ? "Don't have an account? " : 'Already have an account? '}
          <button
            type="button"
            onClick={() => {
              setMode(mode === 'login' ? 'register' : 'login');
              setError('');
            }}
            className="font-medium text-indigo-600 hover:text-indigo-500"
          >
            {mode === 'login' ? 'Create one' : 'Sign in'}
          </button>
        </p>
      </form>
    </div>
  );
}
