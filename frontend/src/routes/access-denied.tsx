import { useNavigate, useSearchParams } from 'react-router';
import { Button } from '@/components/ui/button';
import { useAuth } from '@/auth/use-auth';

/*
 * Public terminal page for a denied session (E44.3 #1827): the user
 * is signed in to GitHub, but no workspace account on this Fishhawk
 * instance admits them (no invited membership and no matching
 * auto-join policy). Sending them back to /login would loop — the
 * OAuth flow succeeds and the membership gate denies again — so the
 * only offered action is signing out to try a different account.
 *
 * E44.31 / #2467: the callback now classifies its denial into a stable
 * `reason` code and carries it on the redirect, so this page explains WHICH
 * branch fired instead of always showing the no-workspace copy. The code is
 * matched against a closed allow-list — the raw parameter is never rendered
 * — and an absent or unrecognized value falls through to the generic body,
 * which is what the client-side RequireAuth navigation (it passes no query)
 * has always shown.
 *
 * NO topology names the login. This page cannot verify who signed in — the
 * value would be a query parameter on an unauthenticated page, so stating
 * "you signed in as X" would let a crafted URL show a misleading login on
 * the deployment's own denial page. fishhawkd's own GET /access-denied (the
 * split-origin topology) is a separate unauthenticated request from the
 * callback and has exactly the same non-authority, so it renders no identity
 * either and the deny redirect no longer carries `provider` or `login` at
 * all; see backend/internal/server/README.md. The test below keeps this
 * pinned against a crafted query.
 */

const DENIAL_REASONS = ['no_membership_resolver', 'no_admitting_account'] as const;
type DenialReason = (typeof DENIAL_REASONS)[number];

function parseReason(raw: string | null): DenialReason | null {
  return DENIAL_REASONS.includes(raw as DenialReason) ? (raw as DenialReason) : null;
}

const EXPLANATION: Record<DenialReason, { why: string; remedy: string }> = {
  no_membership_resolver: {
    why: 'The login gate has no membership resolver wired on this deployment, so every sign-in is denied. This is a deployment-configuration fault, not a problem with your account.',
    remedy:
      'An operator must configure the database and the workspace profile. For a single-tenant self-host, set FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY to the forge login or organization that owns the deployment.',
  },
  no_admitting_account: {
    why: 'No workspace account on this deployment admits your login: there is no invited membership for it, and no auto-join policy matched.',
    remedy:
      'Either set FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY to your login (single-tenant self-host), or ask an existing workspace admin to invite you.',
  },
};

const GENERIC = {
  why: "Your account isn't a member of any workspace on this Fishhawk instance.",
  remedy:
    'Ask a workspace admin to invite you, then sign in again. If every sign-in on this deployment is denied, the login gate has no membership resolver configured and an operator must wire the database.',
};

export function AccessDenied() {
  const { signOut } = useAuth();
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const reason = parseReason(searchParams.get('reason'));
  const copy = reason ? EXPLANATION[reason] : GENERIC;

  async function handleSignOut() {
    await signOut();
    navigate('/login', { replace: true });
  }

  return (
    <div className="flex min-h-full items-center justify-center px-4 py-16">
      <div className="max-w-md space-y-4 text-center">
        <h1 className="text-2xl font-semibold tracking-tight">Access denied</h1>
        <p className="text-sm text-neutral-600 dark:text-neutral-400">{copy.why}</p>
        <p className="text-sm text-neutral-600 dark:text-neutral-400">{copy.remedy}</p>
        <Button variant="outline" onClick={handleSignOut}>
          Sign out
        </Button>
      </div>
    </div>
  );
}
