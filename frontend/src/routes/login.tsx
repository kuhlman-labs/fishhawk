import { Github } from 'lucide-react';
import { Navigate } from 'react-router';
import { Button } from '@/components/ui/button';
import { useAuth } from '@/auth/use-auth';

/*
 * The button is a plain anchor pointing at the backend's OAuth start
 * endpoint, not a fetch — the backend responds with a 302 to GitHub
 * and the browser must follow it. A fetch() would never see the
 * Location header in the way the browser does.
 *
 * If a user lands here while already authenticated (back-button after
 * sign-out, deep link, etc.) we send them home instead of showing
 * a useless "sign in" form.
 */
export function Login() {
  const { status } = useAuth();

  if (status === 'authenticated') {
    return <Navigate to="/" replace />;
  }

  return (
    <div className="flex min-h-full items-center justify-center px-4">
      <div className="w-full max-w-sm space-y-6 rounded-lg border border-neutral-200 bg-white p-8 dark:border-neutral-800 dark:bg-neutral-900">
        <div>
          <h1 className="font-mono text-lg font-semibold tracking-tight">fishhawk</h1>
          <p className="mt-1 text-sm text-neutral-600 dark:text-neutral-400">
            Sign in to review plans and approve workflow runs.
          </p>
        </div>
        <Button asChild className="w-full">
          <a href="/v0/auth/github/login">
            <Github className="size-4" aria-hidden />
            <span>Continue with GitHub</span>
          </a>
        </Button>
      </div>
    </div>
  );
}
