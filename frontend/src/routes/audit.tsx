/*
 * Audit log surface. Brand Foundations §6 calls it a first-class
 * surface; the actual search + verify UI lands later in E7. This
 * placeholder exists so the route renders.
 */
export function Audit() {
  return (
    <section className="space-y-4">
      <header>
        <h1 className="text-xl font-semibold tracking-tight">Audit log</h1>
        <p className="text-sm text-neutral-600 dark:text-neutral-400">
          Append-only, signed record of every approval and run transition.
        </p>
      </header>
      <div className="rounded-md border border-dashed border-neutral-300 p-8 text-sm text-neutral-500 dark:border-neutral-700">
        Audit search lands later in E7.
      </div>
    </section>
  );
}
