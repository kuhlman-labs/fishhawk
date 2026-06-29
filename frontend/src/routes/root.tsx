import { NavLink, Outlet, useNavigate } from 'react-router';
import { ListChecks, LogOut, Network, ScrollText } from 'lucide-react';
import { cn } from '@/lib/cn';
import { Button } from '@/components/ui/button';
import { useAuth } from '@/auth/use-auth';

const navItems = [
  { to: '/runs', label: 'Runs', icon: ListChecks },
  { to: '/campaigns', label: 'Campaigns', icon: Network },
  { to: '/audit', label: 'Audit', icon: ScrollText },
];

export function Root() {
  const { user, signOut } = useAuth();
  const navigate = useNavigate();

  async function handleSignOut() {
    await signOut();
    navigate('/login', { replace: true });
  }

  return (
    <div className="grid min-h-full grid-cols-[14rem_1fr]">
      <aside className="flex flex-col border-r border-neutral-200 bg-neutral-100 px-3 py-4 dark:border-neutral-800 dark:bg-neutral-900">
        <div className="px-2 pb-4 font-mono text-sm font-semibold tracking-tight">fishhawk</div>
        <nav className="flex flex-col gap-1">
          {navItems.map(({ to, label, icon: Icon }) => (
            <NavLink
              key={to}
              to={to}
              end
              className={({ isActive }) =>
                cn(
                  'flex items-center gap-2 rounded-md px-2 py-1.5 text-sm transition-colors',
                  isActive
                    ? 'bg-neutral-200 text-neutral-900 dark:bg-neutral-800 dark:text-neutral-50'
                    : 'text-neutral-700 hover:bg-neutral-200 dark:text-neutral-300 dark:hover:bg-neutral-800',
                )
              }
            >
              <Icon className="size-4" aria-hidden />
              <span>{label}</span>
            </NavLink>
          ))}
        </nav>
        <div className="mt-auto space-y-2 border-t border-neutral-200 pt-3 dark:border-neutral-800">
          {user && (
            <div className="px-2 text-xs text-neutral-600 dark:text-neutral-400">
              <div className="truncate font-medium text-neutral-800 dark:text-neutral-200">
                {user.name || user.github_login}
              </div>
              <div className="truncate">@{user.github_login}</div>
            </div>
          )}
          <Button
            variant="ghost"
            size="sm"
            className="w-full justify-start"
            onClick={handleSignOut}
          >
            <LogOut className="size-4" aria-hidden />
            <span>Sign out</span>
          </Button>
        </div>
      </aside>
      <main className="px-8 py-6">
        <Outlet />
      </main>
    </div>
  );
}
