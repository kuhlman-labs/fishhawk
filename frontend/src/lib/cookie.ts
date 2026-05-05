/*
 * Tiny cookie reader. There's no `document.cookies.get(name)` —
 * the only API is the joined `document.cookie` string. We split,
 * trim, and decode the first matching name; later cookies with the
 * same name (which shouldn't happen with `__Host-` cookies) are
 * ignored.
 *
 * Returns null when:
 *   - the document API isn't available (SSR / vitest without jsdom);
 *   - no cookie with the given name is set.
 */
export function getCookie(name: string): string | null {
  if (typeof document === 'undefined') return null;
  const target = `${name}=`;
  const cookies = document.cookie ? document.cookie.split(';') : [];
  for (const raw of cookies) {
    const c = raw.trim();
    if (c.startsWith(target)) {
      return decodeURIComponent(c.slice(target.length));
    }
  }
  return null;
}
