import { Link } from 'react-router';
import { Button } from '@/components/ui/button';

export function NotFound() {
  return (
    <div className="flex min-h-full items-center justify-center px-4 py-16">
      <div className="space-y-4 text-center">
        <h1 className="text-2xl font-semibold tracking-tight">Not found</h1>
        <p className="text-sm text-neutral-600 dark:text-neutral-400">
          That URL doesn&apos;t resolve to anything in Fishhawk.
        </p>
        <Button asChild variant="outline">
          <Link to="/">Back to runs</Link>
        </Button>
      </div>
    </div>
  );
}
