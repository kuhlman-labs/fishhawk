import { Github } from 'lucide-react';
import { Button } from '@/components/ui/button';

/*
 * Stub sign-in surface. Real OAuth wiring (button → /v0/auth/github/login)
 * lands with E7.2 (#38). Until then the button is non-interactive.
 */
export function Login() {
  return (
    <div className="flex min-h-full items-center justify-center px-4">
      <div className="w-full max-w-sm space-y-6 rounded-lg border border-neutral-200 bg-white p-8 dark:border-neutral-800 dark:bg-neutral-900">
        <div>
          <h1 className="font-mono text-lg font-semibold tracking-tight">fishhawk</h1>
          <p className="mt-1 text-sm text-neutral-600 dark:text-neutral-400">
            Sign in to review plans and approve workflow runs.
          </p>
        </div>
        <Button className="w-full" disabled>
          <Github className="size-4" aria-hidden />
          <span>Continue with GitHub</span>
        </Button>
        <p className="text-xs text-neutral-500 dark:text-neutral-500">
          OAuth wiring pending (E7.2).
        </p>
      </div>
    </div>
  );
}
