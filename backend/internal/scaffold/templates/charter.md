# Charter

**This document is human-authored.** Agents read it; agents never write it.
It is the anchor for prioritization: a backlog grooming run reads it from the
run's base ref, and every ranking it proposes must cite a rubric line below by
id. `fishhawk init` wrote this skeleton — the section structure and the stable
rubric ids — and nothing else. Every `fill me in` marker is yours to replace;
Fishhawk does not draft the direction text for you.

Keep the rubric rows in the shape `| **V1** | <line> |`: a bolded id in the
first cell, the line text in the second. `fishhawk doctor` and the grooming
reader parse exactly that shape. Add, remove or renumber lines as your
repository needs; never reuse a retired id.

---

## 1. North star

<!-- fill me in: the stable framing of what this repository is for, which successive grooming runs reuse instead of re-deriving -->

---

## 2. Current phase

<!-- fill me in: name the current phase and state what "done" means for it -->

### Phase themes

<!-- fill me in: the named themes this phase advances, each with a stable id (T1, T2, ...) -->

### What this phase does *not* require

<!-- fill me in: work that is real value but wrong time for this phase -->

---

## 3. Non-goals

<!-- fill me in: standing commitments about what this repository will not become -->

---

## 4. Prioritization rubric

<!-- fill me in: how the groups below weigh against each other -->

### Value — does it move the current phase?

| id | line |
|---|---|
| **V1** | <!-- fill me in: the highest-value line --> |
| **V2** | <!-- fill me in --> |
| **V3** | <!-- fill me in --> |
| **V4** | <!-- fill me in --> |
| **V5** | <!-- fill me in: real value, wrong time --> |

### Risk — what does deferring it cost?

| id | line |
|---|---|
| **R1** | <!-- fill me in: the costliest deferral --> |
| **R2** | <!-- fill me in --> |
| **R3** | <!-- fill me in --> |
| **R4** | <!-- fill me in --> |
| **R5** | <!-- fill me in --> |

### Dependency unblocking — what does it free?

| id | line |
|---|---|
| **U1** | <!-- fill me in: blocks the most other work --> |
| **U2** | <!-- fill me in --> |
| **U3** | <!-- fill me in --> |
| **U4** | <!-- fill me in: blocks nothing, and nothing blocks it --> |

### Staleness and hygiene — is the item still true?

| id | line |
|---|---|
| **S1** | <!-- fill me in: the body no longer describes the remaining work --> |
| **S2** | <!-- fill me in --> |
| **S3** | <!-- fill me in --> |
| **S4** | <!-- fill me in --> |
| **S5** | <!-- fill me in --> |
