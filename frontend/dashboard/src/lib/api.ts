/**
 * Typed client for the merchant dashboard.
 *
 * The types below mirror the Go structs in internal/dashboard, internal/accounts
 * and internal/checkout. Keeping them in sync is the point: this project has
 * twice shipped a silent contract mismatch — the vault metadata fields and the
 * ML feature vector — where both sides compiled and ran while disagreeing. A
 * renamed Go field now fails the build here instead of rendering as `undefined`.
 *
 * WHAT THIS SENDS, AND WHY IT IS NOT AN API KEY
 *
 * Signing in returns a session token (`sess_...`), and that is what every
 * request below carries. The merchant's secret API key is shown once at
 * creation and then never again — not to this app, not to the server, not to
 * support. The dashboard can therefore not leak a key it does not hold.
 *
 * The session token does live in localStorage, which is readable by any XSS on
 * this page. The better answer is an httpOnly, SameSite cookie; it is not used
 * here because the dashboard (:5173) and the API (:8080) are separate origins
 * in local development, which forces SameSite=None + Secure and does not work
 * over plain http. Deployed behind one domain, this becomes a cookie.
 */

const API_BASE = import.meta.env.VITE_API_URL ?? 'http://localhost:8080';
const SESSION_STORAGE = 'payflow.session';

export function getSession(): string | null {
  return localStorage.getItem(SESSION_STORAGE);
}

export function setSession(token: string): void {
  localStorage.setItem(SESSION_STORAGE, token);
}

export function clearSession(): void {
  localStorage.removeItem(SESSION_STORAGE);
}

export class ApiError extends Error {
  constructor(public status: number, public code: string, message: string) {
    super(message);
  }
}

async function parseError(res: Response): Promise<ApiError> {
  // The API returns a consistent error envelope (§22.2), but a proxy or a
  // network failure can produce something else — so parsing is guarded.
  let code = 'request_failed';
  let message = `Request failed with ${res.status}`;
  try {
    const body = await res.json();
    code = body?.error?.code ?? code;
    message = body?.error?.message ?? message;
  } catch {
    /* non-JSON response; keep the defaults */
  }
  return new ApiError(res.status, code, message);
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  extraHeaders?: Record<string, string>
): Promise<T> {
  const token = getSession();
  const headers: Record<string, string> = { ...extraHeaders };
  if (token) headers.Authorization = `Bearer ${token}`;
  if (body !== undefined) headers['Content-Type'] = 'application/json';

  const res = await fetch(`${API_BASE}${path}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });

  if (!res.ok) {
    const err = await parseError(res);
    // An expired session cannot be recovered by retrying, and leaving the dead
    // token in storage would loop the app through failing requests forever.
    if (res.status === 401 && token) clearSession();
    throw err;
  }
  if (res.status === 204) return undefined as T;
  return res.json() as Promise<T>;
}

function get<T>(path: string): Promise<T> {
  return request<T>('GET', path);
}

// --- types, mirroring internal/dashboard -----------------------------------

export interface Page<T> {
  object: string;
  data: T[];
  has_more: boolean;
  next_cursor?: string;
}

export interface Charge {
  id: string;
  amount: number;
  currency: string;
  status: 'pending' | 'succeeded' | 'failed' | 'requires_action' | 'requires_reconciliation';
  failure_code?: string;
  card_last4?: string;
  card_brand?: string;
  risk_level?: 'low' | 'medium' | 'high';
  risk_score?: number;
  amount_refunded: number;
  disputed: boolean;
  created_at: string;
}

// Mirrors payments.Refund. Note `status`: a refund can land in
// requires_reconciliation exactly like a charge, and the UI must not present
// that as either success or failure.
export interface Refund {
  id: string;
  object: string;
  charge_id: string;
  amount: number;
  currency: string;
  status: 'pending' | 'succeeded' | 'failed' | 'requires_reconciliation';
  reason?: string;
  failure_code?: string;
  created_at: string;
}

export interface Balance {
  currency: string;
  available: number;
  pending: number;
  reserved: number;
  in_transit: number;
}

export interface Summary {
  window_days: number;
  gross_volume: number;
  net_volume: number;
  currency: string;
  succeeded_count: number;
  failed_count: number;
  success_rate: number;
  refunded: number;
  disputed_count: number;
  unresolved_count: number;
}

export interface VolumePoint {
  date: string;
  volume: number;
  count: number;
}

export interface Dispute {
  id: string;
  charge_id: string;
  amount: number;
  currency: string;
  reason: string;
  status: 'needs_response' | 'under_review' | 'won' | 'lost';
  evidence_due_by: string;
  created_at: string;
}

export interface Payout {
  id: string;
  amount: number;
  currency: string;
  status: 'pending' | 'paid' | 'failed' | 'requires_reconciliation';
  failure_code?: string;
  paid_at?: string;
  created_at: string;
}

export interface WebhookDelivery {
  id: string;
  event_id: string;
  event_type: string;
  status: 'pending' | 'delivered' | 'failed' | 'dead';
  attempts: number;
  last_status_code?: number;
  last_error?: string;
  next_attempt_at?: string;
  created_at: string;
}

// --- accounts, mirroring internal/accounts ---------------------------------

export interface User {
  id: string;
  merchant_id: string;
  email: string;
  name: string;
  business_name: string;
  country: string;
  currency: string;
  publishable_key: string;
  created_at: string;
}

export interface AuthSession {
  token: string;
  expires_at: string;
  user: User;
}

export interface ApiKey {
  id: string;
  name: string;
  prefix: string;
  mode: 'test' | 'live';
  scope: 'read' | 'write';
  created_at: string;
  last_used_at?: string;
  revoked_at?: string;
}

// --- payment links, mirroring internal/checkout ----------------------------

export interface PaymentLink {
  id: string;
  object: string;
  code: string;
  url: string;
  amount: number;
  currency: string;
  name: string;
  description: string;
  success_url?: string;
  cancel_url?: string;
  active: boolean;
  created_at: string;
  payments: number;
  volume: number;
}

// --- endpoints -------------------------------------------------------------

export const auth = {
  register: (body: {
    business_name: string;
    email: string;
    password: string;
    name?: string;
    country?: string;
    currency?: string;
  }) => request<{ session: AuthSession; api_key: string }>('POST', '/v1/auth/register', body),

  login: (email: string, password: string) =>
    request<AuthSession>('POST', '/v1/auth/login', { email, password }),

  logout: () => request<void>('POST', '/v1/auth/logout'),

  me: () => get<User>('/v1/account'),
};

export const api = {
  /**
   * Refund a charge, fully or partially.
   *
   * `idempotencyKey` is passed in by the caller rather than generated here, and
   * that is the whole contract. The key must identify the refund INTENT — one
   * key per decision to refund — so that a double-clicked button, a dropped
   * response and a retried fetch all resolve to one refund. Generating a fresh
   * key inside this function would make every retry a new refund, which is
   * exactly how a customer gets their money back twice.
   *
   * Omit `amount` to refund whatever remains on the charge.
   */
  refund: (chargeId: string, idempotencyKey: string, amount?: number) =>
    request<Refund>(
      'POST',
      '/v1/refunds',
      { charge: chargeId, ...(amount ? { amount } : {}) },
      { 'Idempotency-Key': idempotencyKey }
    ),

  keys: () => get<{ data: ApiKey[] }>('/v1/account/api_keys'),
  // The raw key comes back exactly once, here. There is no endpoint that can
  // return it again, by design.
  createKey: (name: string, scope: 'read' | 'write') =>
    request<{ key: string; warning: string }>('POST', '/v1/account/api_keys', {
      name,
      mode: 'test',
      scope,
    }),
  revokeKey: (id: string) => request<void>('POST', `/v1/account/api_keys/${id}/revoke`),

  links: () => get<{ data: PaymentLink[] }>('/v1/payment_links'),
  createLink: (body: {
    amount: number;
    currency: string;
    name: string;
    description?: string;
    success_url?: string;
  }) => request<PaymentLink>('POST', '/v1/payment_links', body),
  setLinkActive: (id: string, active: boolean) =>
    request<void>('POST', `/v1/payment_links/${id}/${active ? 'activate' : 'deactivate'}`),

  summary: (days = 30) => get<Summary>(`/v1/dashboard/summary?days=${days}`),
  volume: (days = 30) => get<{ data: VolumePoint[] }>(`/v1/dashboard/volume?days=${days}`),
  balances: () => get<{ data: Balance[] }>('/v1/dashboard/balances'),
  charges: (params: { status?: string; cursor?: string; limit?: number } = {}) => {
    const q = new URLSearchParams();
    if (params.status) q.set('status', params.status);
    if (params.cursor) q.set('cursor', params.cursor);
    if (params.limit) q.set('limit', String(params.limit));
    return get<Page<Charge>>(`/v1/dashboard/charges?${q}`);
  },
  charge: (id: string) => get<Charge>(`/v1/dashboard/charges/${id}`),
  disputes: () => get<Page<Dispute>>('/v1/dashboard/disputes'),
  payouts: () => get<Page<Payout>>('/v1/dashboard/payouts'),
  deliveries: (status?: string) =>
    get<Page<WebhookDelivery>>(
      `/v1/dashboard/webhook_deliveries${status ? `?status=${status}` : ''}`
    ),
};

/**
 * Format minor units for display.
 *
 * Amounts are integers everywhere in this system and are divided exactly once,
 * here, at the edge, for a human to read. Nothing downstream consumes this
 * value — dividing any earlier is how float drift gets into a ledger.
 */
export function formatMoney(cents: number, currency = 'USD'): string {
  return new Intl.NumberFormat('en-US', { style: 'currency', currency }).format(
    cents / 100
  );
}

export function formatDate(iso: string): string {
  return new Date(iso).toLocaleString('en-US', {
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
  });
}
