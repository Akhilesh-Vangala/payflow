import { useEffect, useState } from 'react';
import { QueryClient, QueryClientProvider, useQuery } from '@tanstack/react-query';
import { NavLink, Navigate, Route, HashRouter as Router, Routes } from 'react-router-dom';
import { auth, clearSession, getSession, type User } from './lib/api';
import { Auth } from './pages/Auth';
import { Overview } from './pages/Overview';
import { Charges } from './pages/Charges';
import { Disputes, Payouts, Webhooks } from './pages/Activity';
import { Keys } from './pages/Keys';
import { Links } from './pages/Links';
import { Setup } from './pages/Setup';

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // Financial data should not be stale for long, but nor should every tab
      // focus hammer the API. Ten seconds is short enough that a merchant
      // watching a charge land sees it, long enough to avoid a request storm.
      staleTime: 10_000,
      // A 401 means the session is gone; retrying cannot fix it and just delays
      // the sign-in prompt the user needs to see.
      retry: (count, error) =>
        count < 2 && !(error instanceof Error && error.message.includes('401')),
    },
  },
});

const NAV = [
  { to: '/overview', label: 'Overview' },
  { to: '/charges', label: 'Charges' },
  { to: '/links', label: 'Payment links' },
  { to: '/disputes', label: 'Disputes' },
  { to: '/payouts', label: 'Payouts' },
  { to: '/webhooks', label: 'Webhooks' },
  { to: '/keys', label: 'API keys' },
  { to: '/setup', label: 'Set up' },
];

function Shell({ user, onSignOut }: { user: User; onSignOut: () => void }) {
  return (
    <div className="min-h-screen bg-slate-50">
      <header className="border-b border-slate-200 bg-white">
        <div className="mx-auto flex max-w-6xl items-center justify-between gap-6 px-6 py-3">
          <div className="flex min-w-0 items-center gap-6">
            <span className="shrink-0 text-sm font-semibold text-slate-900">PayFlow</span>
            <nav className="flex flex-wrap gap-1">
              {NAV.map((n) => (
                <NavLink
                  key={n.to}
                  to={n.to}
                  className={({ isActive }) =>
                    `rounded-md px-3 py-1.5 text-sm font-medium transition ${
                      isActive
                        ? 'bg-slate-100 text-slate-900'
                        : 'text-slate-600 hover:bg-slate-50 hover:text-slate-900'
                    }`
                  }
                >
                  {n.label}
                </NavLink>
              ))}
            </nav>
          </div>

          <div className="flex shrink-0 items-center gap-3">
            <span className="hidden text-xs text-slate-500 sm:inline">
              {user.business_name}
            </span>
            <span className="rounded bg-amber-50 px-1.5 py-0.5 text-[11px] font-medium text-amber-800 ring-1 ring-inset ring-amber-600/20">
              test mode
            </span>
            <button
              onClick={onSignOut}
              className="text-xs font-medium text-slate-500 hover:text-slate-900"
            >
              Sign out
            </button>
          </div>
        </div>
      </header>

      <main className="mx-auto max-w-6xl px-6 py-8">
        <Routes>
          <Route path="/overview" element={<Overview />} />
          <Route path="/charges" element={<Charges />} />
          <Route path="/links" element={<Links />} />
          <Route path="/disputes" element={<Disputes />} />
          <Route path="/payouts" element={<Payouts />} />
          <Route path="/webhooks" element={<Webhooks />} />
          <Route path="/keys" element={<Keys />} />
          <Route path="/setup" element={<Setup />} />
          <Route path="*" element={<Navigate to="/overview" replace />} />
        </Routes>
      </main>
    </div>
  );
}

/**
 * Resolves a stored session token to a user before rendering the app.
 *
 * A token in localStorage is not proof of anything — it may have expired, been
 * revoked from another device, or belong to a database that has since been
 * reset. Asking the server who it belongs to is the only way to know, and doing
 * it here means no page has to handle a half-authenticated state.
 */
function Authenticated({ onFailed }: { onFailed: () => void }) {
  const { data: user, isLoading, isError } = useQuery({
    queryKey: ['me'],
    queryFn: auth.me,
    retry: false,
  });

  useEffect(() => {
    if (isError) {
      clearSession();
      onFailed();
    }
  }, [isError, onFailed]);

  if (isLoading) {
    return (
      <div className="flex min-h-screen items-center justify-center text-sm text-slate-400">
        Loading…
      </div>
    );
  }
  if (!user) return null;

  return (
    <Router>
      <Shell
        user={user}
        onSignOut={async () => {
          // Revoke server-side first, so the token is dead even if this browser
          // keeps a copy. Failure is ignored: the local session is cleared
          // either way, and a logout that can visibly fail encourages clients
          // to hold on to the token when it does.
          try {
            await auth.logout();
          } catch {
            /* revoked on the next expiry regardless */
          }
          clearSession();
          onFailed();
        }}
      />
    </Router>
  );
}

export default function App() {
  const [signedIn, setSignedIn] = useState(() => Boolean(getSession()));

  if (!signedIn) {
    return (
      <QueryClientProvider client={queryClient}>
        <Auth
          onSignedIn={() => {
            queryClient.clear();
            setSignedIn(true);
          }}
        />
      </QueryClientProvider>
    );
  }

  return (
    <QueryClientProvider client={queryClient}>
      <Authenticated
        onFailed={() => {
          queryClient.clear();
          setSignedIn(false);
        }}
      />
    </QueryClientProvider>
  );
}
