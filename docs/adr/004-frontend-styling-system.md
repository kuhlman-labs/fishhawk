---
id: ADR-004
title: "Frontend styling system"
status: accepted
date: 2026-04-30
issue: https://github.com/kuhlman-labs/fishhawk/issues/68
supersedes: []
superseded_by: []
applies_to: ["frontend/src/components/ui/**"]
---

# ADR-004: Frontend styling system

## Context

The frontend is Vite + React Router (per session decision). A styling system is needed. Brand Foundations §6 calls for restraint, density, and the audit log as a first-class surface — strong opinions, but not yet specific colors or components.

## Options

- **Tailwind + shadcn/ui** — most common 2025/2026 default for B2B dashboards. Headless components, copy-into-repo (no npm dep churn), Tailwind utility-first matches the "restraint" voice when used carefully. Mature ecosystem.
- **CSS Modules + custom design tokens** — full control, no framework. Slower to ship; requires more design judgment.
- **Panda CSS** — typed CSS-in-JS with build-time extraction. Newer; less ecosystem support.
- **Mantine / Chakra / Material UI** — full component libraries. Faster scaffold; harder to make distinctive (Brand §6 explicitly warns against generic SaaS look).

## Recommendation

Tailwind + shadcn/ui. Lets us ship fast with a strong default while keeping the door open for distinctive components later. shadcn's "copy into repo" model means we can deviate from defaults without fighting a library.

## Decision

**Recorded 2026-04-30: Tailwind CSS v4 + shadcn/ui.**

- **Tailwind v4** with the Vite plugin (`@tailwindcss/vite`). Config-as-CSS via `@theme` blocks; lighter than the v3 JS config.
- **shadcn/ui** components copied into `frontend/src/components/ui/` as needed. Not a dependency; we own the source.
- **Radix UI primitives** under shadcn/ui for accessibility (focus management, keyboard handling). Direct dependency.
- **lucide-react** for icons (small, tree-shakable, the shadcn default).
- **No additional component library** in v0. If a domain-specific thing (e.g., audit log search UI) needs custom components, we build them on Radix primitives directly.

Color and typography decisions are still in Brand Foundations §11 (designer engagement) territory; for v0 we'll use shadcn's neutral palette and Inter, and revisit when a designer is engaged.

## Consequences

**Easier**
- Shipping the plan-review and audit-log surfaces doesn't get blocked on UI library decisions.
- shadcn-copied components are inspectable, modifiable, and version-controlled — no opaque dependency upgrades breaking the UI.
- Tailwind utility-first matches the "density appropriate to senior engineers" UI principle from Brand Foundations §6.

**Harder**
- Tailwind class strings can grow long in JSX. Mitigate with `clsx` + extracting compound patterns into named components.
- Tailwind v4's CSS-first config is newer; expect occasional rough edges with tooling.

**Other decisions this constrains**
- E7.1 (#37) frontend scaffold installs Tailwind + shadcn during initial setup.
- The brand visual identity work (when it happens) feeds into the Tailwind theme tokens.

## Spec reference

`docs/BRAND_FOUNDATIONS.md` §6

## Target deadline

Day 5 — **met**.

---
Parent epic: #15
