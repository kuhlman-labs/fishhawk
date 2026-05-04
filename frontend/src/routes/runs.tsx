/*
 * Runs list. Real list + plan review surfaces land with E7.3 (#56)
 * and E7.4 (#57). For now this is a labeled placeholder so the
 * route renders and the nav highlight works.
 */
export function Runs() {
  return (
    <section className="space-y-4">
      <header>
        <h1 className="text-xl font-semibold tracking-tight">Runs</h1>
        <p className="text-sm text-neutral-600 dark:text-neutral-400">
          Workflow runs across your repositories.
        </p>
      </header>
      <div className="rounded-md border border-dashed border-neutral-300 p-8 text-sm text-neutral-500 dark:border-neutral-700">
        Runs list lands with E7.3 / E7.4.
      </div>
    </section>
  );
}
