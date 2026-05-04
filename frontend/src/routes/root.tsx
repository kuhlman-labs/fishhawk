import { NavLink, Outlet } from 'react-router';
import { ListChecks, ScrollText } from 'lucide-react';
import { cn } from '@/lib/cn';

const navItems = [
  { to: '/runs', label: 'Runs', icon: ListChecks },
  { to: '/audit', label: 'Audit', icon: ScrollText },
];

export function Root() {
  return (
    <div className="grid min-h-full grid-cols-[14rem_1fr]">
      <aside className="border-r border-neutral-200 bg-neutral-100 px-3 py-4 dark:border-neutral-800 dark:bg-neutral-900">
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
      </aside>
      <main className="px-8 py-6">
        <Outlet />
      </main>
    </div>
  );
}
