# Fishhawk — Brand Foundations

> **Status:** Draft v0.1
> **Owner:** Brett
> **Last revised:** 2026-04-30
> **Purpose:** The strategic and creative foundation for Fishhawk's brand identity. This document gives designers, copywriters, and the founder a shared reference for what Fishhawk is, how it should feel, and how to make consistent choices about voice, visual identity, and presentation.
>
> This is not a final brand guide. A final brand guide is produced *after* visual identity work is complete (logo, color system, typography). This is the brief that produces it.

---

## 1. The product, in one sentence

Fishhawk is the workflow and governance layer for agent-driven software development. It gives engineering teams an opinionated, auditable process for how AI agents plan, implement, and ship changes — without locking them into any specific agent, tracker, or stack.

## 2. The name

### Origin

Fishhawk is named after Lithia, Florida, where the founder lives. The neighborhood and the surrounding area are home to ospreys — known colloquially in the American South as fishhawks. The bird is a real, present part of the place where the company was started.

### Why the name works

- **Memorable.** Concrete, two-syllable, easy to say and spell after hearing once.
- **Founder-anchored.** A real place, a real bird, a real story. Not manufactured for marketing.
- **Latent metaphor.** The osprey hovers patiently, watches carefully, dives with precision. The audit/governance product mirrors this: agents act under careful human oversight, with structured precision. The metaphor is available if needed; it doesn't have to be foregrounded.
- **Doesn't telegraph the v0 scope.** Unlike "Audit," "Sentinel," or "Warden," Fishhawk doesn't pre-commit the brand to "we are a governance product." This matters because v0 is governance and v1+ is full SDLC orchestration. The name grows with the product.
- **Right register for developer tools.** Quirky, specific, slightly idiosyncratic — the same family as Datadog, Snyk, Mailchimp, Hashicorp, Kubernetes. Distinctive in a sea of generic Latinate enterprise names.

### Casing and spelling

The canonical rendering is **Fishhawk** — one word, single capital F, no hyphen, no internal capital.

- Correct: Fishhawk
- Acceptable in lowercase wordmarks: fishhawk
- Incorrect: FishHawk, Fish Hawk, Fish-Hawk, FISHHAWK (except in deliberate typographic contexts)

In running prose, always "Fishhawk." In a wordmark, the wordmark style governs (likely lowercase; see Section 6).

The product is referred to as Fishhawk, not "the Fishhawk platform" or "Fishhawk.ai." In rare cases where disambiguation is needed (a casual reader unfamiliar with the brand), "Fishhawk, an AI workflow governance platform" is acceptable on first reference.

### Pronunciation

FISH-hawk. The two syllables get roughly equal stress. Slight elision is natural in speech ("fish-awk") but the spelling is always two h's.

---

## 3. Positioning

### What Fishhawk is

The opinionated workflow and governance layer for engineering teams using AI coding agents. The product where teams encode their process for how AI does work, enforce that process automatically, and prove afterward what was done and why.

### What Fishhawk is not

- A coding agent (Fishhawk orchestrates them; it doesn't compete with Claude Code, Cursor, Copilot).
- A project management tool (the customer's tracker is source of truth).
- A CI/CD platform (Fishhawk runs on the customer's CI).
- A general-purpose workflow engine (Fishhawk is opinionated for software development specifically).
- A monitoring or incident response tool (out of scope for v0).

### The core promise

> Your agents do the work. Your team approves the work. Fishhawk holds the record.

### What we believe

Three convictions that should shape every brand expression:

1. **Humans direct, agents implement.** The future of software is human judgment plus agent execution. Fishhawk is built around this asymmetry — the workflow encodes which decisions belong to humans and which to agents, permanently.

2. **Governance is the product, not a feature.** Most AI tools treat audit and policy as compliance bolt-ons. Fishhawk treats them as the core abstraction. The workflow spec, the audit log, and the approval gates are not afterthoughts; they are the system.

3. **Opinionated tools beat flexible ones.** The flexibility of LangGraph or generic workflow engines is the wrong answer for software governance. Fishhawk has strong opinions about how agent-driven work should be structured. Customers adopt those opinions; the opinions become their process.

### Positioning against incumbents

| Competitor | Their angle | Fishhawk's wedge |
|---|---|---|
| IBM Bob | Full-stack SDLC orchestration, IBM-aligned | Tool-agnostic, opinionated workflow, OSS-friendly |
| GitHub Copilot agent | Tightly integrated, single-vendor | Multi-agent governance layer above |
| AWS DevOps Agent | AWS-centric ops + agent | Multi-stack; planning + implementation, not just ops |
| LangGraph / CrewAI | Generic agent orchestration | Opinionated for SDLC specifically; governance built-in |
| JetBrains Central | Agnostic agent platform with policy | Smaller, sharper, OSS-distributed; audit-first |

---

## 4. Audience

### Primary buyer

VP Engineering, Director of Platform, Head of DevX at a mid-sized engineering organization (50–300 engineers). Compliance and security are influencers; engineering owns the budget.

The buyer is sophisticated, time-poor, has been pitched dozens of AI tools in the last year, and is skeptical of marketing claims. They respond to specificity, technical credibility, and honest framing of trade-offs. They are turned off by jargon, hype, and over-promising.

### Primary user

Tech leads and senior engineers at the same organizations. They are the ones who will write `.fishhawk/workflows.yaml`, approve plans, and live with the product day to day. They are similarly sophisticated, similarly skeptical, and value tools that respect their time and intelligence.

### Audience principle

Fishhawk speaks to senior engineers who have seen tools come and go. Treat them as peers. Don't condescend, don't oversell, don't pad. Show them the substance and let them decide.

---

## 5. Voice and tone

### Voice principles

**Direct.** Lead with the answer, not the framing. Avoid hedging, avoid corporate softening. "Fishhawk runs on your CI" beats "Fishhawk is designed to be deployable in your existing CI environment."

**Honest about trade-offs.** Every product has costs. Acknowledge them. "This adds friction — that's the point" is more credible than "frictionless governance."

**Technical without being jargon-laden.** Use precise technical language where it adds clarity. Avoid technical-sounding language that obscures meaning. "Plans are persisted to the audit log" is good; "AI-powered workflow orchestration leverages enterprise-grade observability" is bad.

**Confident without being grandiose.** Fishhawk is good at what it does; we don't need to claim it will revolutionize the industry. Understatement reads as more confident than overclaim, especially to senior engineers.

**Quietly idiosyncratic.** The name is unusual; the company can be too. A small, dignified note about the bird on the about page. A footer that mentions Lithia, Florida. A light personality that signals "real people made this in a real place."

### Things we never say

- "Revolutionary," "game-changing," "next-generation," "industry-leading," "world-class"
- "AI-powered" as a sole differentiator (it's table stakes; everyone is)
- "Frictionless," "seamless," "effortless" (governance has friction; that's the value)
- "Trust" as marketing claim ("trust Fishhawk with your AI governance" — show, don't claim)
- "Empower" in any context
- Anything that sounds like every other compliance vendor's homepage

### Tone calibration by surface

**Landing page / marketing site.** Confident, specific, light enough to feel human. Lead with concrete capability statements; back them with credible technical detail. Include a small note of warmth (the founder story, the name origin) that differentiates from generic enterprise voice.

**Product UI.** Direct, brief, helpful. Error messages are honest. Confirmations are minimal. Onboarding doesn't celebrate; it gets the user to first value.

**Documentation.** Patient and thorough. Match the reader's expertise level. Examples over abstractions.

**Sales conversations.** Substantive and respectful of the buyer's intelligence. The buyer is evaluating you against IBM Bob and AWS DevOps Agent; meet them at that level.

**Technical writing (blog, RFCs, design docs).** Precise, sometimes opinionated. Treat readers as peers. Show your reasoning.

**Founder voice (Twitter, blog posts, speaking).** Personal, specific, occasionally idiosyncratic. The founder lives in Lithia. The founder named the company after the bird outside their window. That's allowed to come through.

### Voice examples

**Landing page hero — good:**
> The workflow and governance layer for AI-driven software development. Agents do the work. Your team approves the work. Fishhawk holds the record.

**Landing page hero — bad:**
> Fishhawk empowers enterprise engineering teams to harness the power of AI agents with industry-leading governance, observability, and compliance.

**Feature description — good:**
> Plans live in your project tracker. Fishhawk renders them as comments on the originating GitHub issue, kept in sync with the canonical version in the audit log. Your team reviews where they already work; the record stays where it belongs.

**Feature description — bad:**
> Fishhawk's intelligent plan synchronization seamlessly integrates with your existing project management workflows, ensuring full visibility and traceability across stakeholders.

**Error message — good:**
> Plan stage failed: agent exceeded token budget (limit 200,000; used 247,300). Retry with a more constrained scope, or raise the budget in `.fishhawk/workflows.yaml`.

**Error message — bad:**
> Oops! Something went wrong. Please try again or contact support.

---

## 6. Visual identity (direction, not specification)

### Logo direction

For v0, **wordmark-only** is recommended. Linear, Stripe, Vercel — all wordmark-first, and they project competence without relying on a mark. A premature mark is a common founder time-sink.

Recommended approach for the wordmark:

- **Casing:** all lowercase ("fishhawk") for the wordmark. Matches the modern dev-tools convention. Sentence-case "Fishhawk" stays canonical for prose.
- **Weight:** medium or semibold. Not light (reads as luxury/lifestyle). Not heavy (reads as utility/industrial).
- **Letterforms:** geometric sans-serif with a slight humanist warmth. Avoid pure geometric (too cold for a name with personality). Avoid heavy display fonts (too loud).
- **Custom touches:** consider a subtle modification — a slightly distinctive 'h,' a custom kerning of the double-h. Don't overdo it. Restraint beats cleverness.

A pictorial mark can come later (v1 or v2). When it does:

- **Avoid literal birds.** Twitter, Duolingo, Mailchimp, every fintech ever. A literal osprey logo puts Fishhawk in crowded visual company.
- **If a mark is desired, prefer abstract.** A geometric form that suggests precision, focus, dive, or watching — without being a bird. A triangular dive arc, a focused crosshair, an abstract wing form. Symbolism, not illustration.
- **Avoid the obvious shield/lock/checkmark security tropes.** Fishhawk is not a security product in visual identity, even though governance is part of what it does.

### Color direction

The color system should signal *technical seriousness with a hint of place.*

**Recommended palette direction (for a designer to refine):**

- **Primary:** A deep, slightly desaturated blue-green. Not navy (too corporate). Not teal (too consumer-tech). Something closer to a deep cypress green or a slate blue with green undertones — earned from the Florida natural-world reference without being on-the-nose.
- **Secondary / accent:** A warm neutral — sand, oat, parchment — for backgrounds and quiet surfaces. Provides warmth and prevents the brand from feeling clinical.
- **Functional accent:** A single bright color used sparingly for calls-to-action and approvals. Suggested direction: a warm amber or rust-orange, drawn from sunset/clay tones rather than the typical SaaS-bright orange. Used for *approval* states specifically — the affirmative human action that makes Fishhawk go.
- **Status colors:** Standard semantic palette (red for errors, amber for warnings, green for success), but desaturated to match the overall palette.

What to avoid:

- Bright tech blues (#0066FF and family) — over-used, feels like every SaaS product.
- Pure black on pure white — feels stark and inhuman. Use very dark blue-black or warm-gray blacks against off-white.
- Gradients as a primary visual treatment — feels dated and decorative.
- "Compliance vendor purple" or "security vendor red" — Fishhawk is not those products.

### Typography direction

A two-family system, common for modern dev-tools brands:

- **Display / wordmark / headlines:** A geometric humanist sans. Examples to consider as starting points: Inter (default-good), GT America (more character), Söhne (refined and confident), Söhne Mono for code. Open-source first; the founder ships OSS and the type system should match that ethos.
- **Body / UI:** Inter is the safe and excellent default. Pair with a quality monospace for code (JetBrains Mono, IBM Plex Mono, or Berkeley Mono if budget allows).

What to avoid:

- Serif body type (signals editorial / publishing, wrong category).
- Display fonts in body copy.
- More than two families across the system.

### Photography and imagery

Limited use of photography for v0. When used:

- **Avoid stock photography.** Especially the "diverse team in modern office" cliché. It signals lack of identity.
- **Avoid AI-generated imagery.** Particularly for an AI governance product — the optics are bad, and the visual quality of generated brand imagery in 2026 is still inconsistent.
- **If imagery is desired,** consider one of: high-quality nature photography from Florida (cypress, water, sky — not necessarily birds, restraint matters), abstract architectural photography (precision, structure), or no imagery at all and let typography and color carry the visual identity.

### Product UI principles

The application UI should match the brand voice: direct, technical, calm. Specific principles:

- **Information density appropriate to the user.** Senior engineers can handle dense information; don't over-simplify.
- **Restraint with color.** Color is used to communicate state (approved, pending, failed), not to decorate.
- **Typographic hierarchy over decorative dividers.** Whitespace, type weight, and scale do most of the work.
- **The audit log is a first-class surface.** It should feel substantial and queryable, not buried in a settings menu. The audit log is the product's most distinctive surface; design it like a tool, not like a side feature.
- **Plans are documents, not chat messages.** The plan review surface should feel like reviewing a technical document — structured, navigable, diffable. Not like reviewing a chat transcript.
- **Status and approval are prominent.** A workflow run in flight should make clear, from any view, where it is and what's needed next. The approval surface is high-stakes and deserves design attention.

---

## 7. Naming conventions for product surfaces

Consistency on internal naming reduces friction and builds the brand.

- The product is **Fishhawk** (proper noun). Not "the Fishhawk platform," not "Fishhawk app."
- The CLI is **`fishhawk`** (lowercase, used as the command name). Examples: `fishhawk validate`, `fishhawk run start`.
- The configuration directory in customer repos is **`.fishhawk/`** (lowercase, dot-prefixed).
- The primary spec file is **`.fishhawk/workflows.yaml`**.
- The GitHub Action is **`fishhawk/runner@v1`** (versioned, lowercase).
- The web product surface is **app.fishhawk.[tld]** (UI), **fishhawk.[tld]** (marketing), **docs.fishhawk.[tld]** (documentation).

For features and concepts in the workflow spec, use lowercase noun forms: *workflow*, *stage*, *gate*, *constraint*, *approver*, *artifact*, *plan*, *audit log*. Don't capitalize them as if they were branded ("the Plan Artifact™") — they are technical primitives.

For published artifacts (the runner action, the SDK, the spec):

- Versioned semver from day one (v0.1 → v1.0 → ...).
- Workflow spec versions are independent (`schema: standard_v1`) and never break old plans in the audit log.

---

## 8. Story and origin

The founder story is part of the brand. It should be visible (about page, README, occasional reference in marketing) without being center-stage.

### The short version (one-liner for footers, social bios)

> Built in Lithia, Florida.

### The medium version (about page paragraph)

> Fishhawk was built in Lithia, Florida — a community named after the ospreys that nest in the cypress trees here. The bird is patient, watchful, and precise. It hovers, it watches, it dives. It's a fitting namesake for a product about governance: agents do the work, but humans hover, watch, and approve. The neighborhood is real. The birds are real. The product is built by one person, alongside the agents it governs.

### The longer version (founder blog post / narrative)

Reserved for the founder's first substantive blog post or interview. Threads the personal (the place), the technical (what Fishhawk does), and the conviction (why opinionated governance for AI agents matters now). Should be written in the founder's actual voice.

---

## 9. The methodology brand

Fishhawk is built using Fishhawk. This is not a marketing line; it is a constraint of the product, and it should be visible in the brand.

- The `.fishhawk/workflows.yaml` for Fishhawk's own development is open in the public repository.
- The audit log of Fishhawk's own development is published as a public artifact.
- The first substantive blog post explains how Fishhawk is built using Fishhawk, with autonomy tiers and concrete examples.
- Marketing language about "built by agents" is *specific* and *verifiable*. Example: "73% of merged PRs in Q3 were implemented end-to-end by Claude Code under our medium-autonomy workflow, with human plan approval and human PR review." Avoid vague claims like "built with AI."

This methodology should land as a feature of the brand, not as a gimmick. The honest version reads as quietly confident; the marketing version reads as overclaim. Always pick the honest version.

---

## 10. What's deliberately deferred

These are real branding decisions that should not be made yet:

- **Pictorial logo / mark.** Defer until v1 or until a designer with the right taste is engaged. Wordmark-only is correct for v0.
- **Full color system with hex codes and accessibility ratings.** A designer should produce this once the wordmark is set.
- **Custom typography.** Use open-source defaults (Inter + a monospace) until and unless a designer recommends licensed type. Don't license type prematurely.
- **Brand photography style guide.** Defer until photography is actually being used.
- **Motion / animation guidelines.** Defer until the product or marketing site needs motion in a meaningful way.
- **Sub-brand or product-line naming.** Defer until there's a second product or tier worth naming. Premature sub-branding fragments attention.
- **Mascot or brand character.** Recommended: never. The osprey is referenced; it does not need a Fishhawk mascot named "Ozzie."

---

## 11. Decisions to make before designer engagement

When a designer is engaged to produce the actual brand guide, they will need answers to these questions. Founders should think through them before the kickoff:

1. **Wordmark casing:** "fishhawk" (all lowercase) or "Fishhawk" (sentence case) or "FISHHAWK" (all caps)? Recommended: lowercase wordmark, sentence-case in prose.
2. **Primary color:** approximate direction is Florida-cypress-green / slate-blue with warm-neutral support. Designer will refine to exact values.
3. **Typography:** open-source defaults (Inter, JetBrains Mono) for v0, or licensed type? Recommended: open-source for v0.
4. **Domain and TLD:** which is canonical? `.com`, `.dev`, `.ai`, `.io`? Recommended: `.dev` or `.ai` for the primary product surface; acquire `.com` if available at reasonable cost for redirect.
5. **OSS vs. hosted brand split:** is the OSS project branded the same as the hosted product, or differently? Recommended: same brand, with hosted product distinguished by `app.fishhawk.[tld]` or similar.
6. **Founder visibility:** how prominent is the founder in early brand expressions? Recommended: visible but not dominant — about page, occasional blog post, conference talks. The brand is Fishhawk, not the founder.

---

## 12. What this document is not

- A finished brand guide. (That comes after design work.)
- A logo. (No logo exists yet.)
- A color system. (Direction only; exact values come from a designer.)
- A copywriting standard. (Voice principles only; full copy guidelines come later.)
- A locked-in commitment. (Decisions in this document may change as the brand develops; this document is updated when they do.)

When this document and reality disagree: update this document. When this document and a final designer-produced brand guide disagree: the designer's guide wins for visual decisions; this document remains canonical for voice, naming, and positioning.

---

*End of brand foundations.*
