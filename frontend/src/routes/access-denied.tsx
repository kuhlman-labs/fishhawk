import { useNavigate } from 'react-router';
import { Button } from '@/components/ui/button';
import { useAuth } from '@/auth/use-auth';

/*
 * Public terminal page for a denied session (E44.3 #1827): the user
 * is signed in to GitHub, but no workspace account on this Fishhawk
 * instance admits them (no invited membership and no matching
 * auto-join policy). Sending them back to /login would loop — the
 * OAuth flow succeeds and the membership gate denies again — so the
 * only offered action is signing out to try a different account.
 */
export function AccessDenied() {
  const { signOut } = useAuth();
  const navigate = useNavigate();

  async function handleSignOut() {
    await signOut();
    navigate('/login', { replace: true });
  }

  return (
    <div className="flex min-h-full items-center justify-center px-4 py-16">
      <div className="max-w-md space-y-4 text-center">
        <h1 className="text-2xl font-semibold tracking-tight">Access denied</h1>
        <p className="text-sm text-neutral-600 dark:text-neutral-400">
          Your account isn&apos;t a member of any workspace on this Fishhawk instance. Ask a
          workspace admin to invite you, then sign in again.
        </p>
        <Button variant="outline" onClick={handleSignOut}>
          Sign out
        </Button>
      </div>
    </div>
  );
}
