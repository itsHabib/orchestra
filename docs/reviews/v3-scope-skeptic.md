# v3 Scope-Skeptic Review

Reviewer role: ruthless YAGNI. Every criticism has a section ref.

---

## C1: gh-auth was THE blocker — v3 doesn't solve it first

**Evidence:** `feedback-mcp-server.md §§ "Tick @ 16 min"` — both teams stalled on `gh auth status` returning empty. Neither shipped a PR. That is the terminal failure of the only real run.

**What v3 does:** Phase A ships `team→agent` rename and observability fields. gh-auth is deferred to "Phase A or B depending on urgency" (§12.1 open questions). It is a P0 blocker for every "ship a PR" recipe, which is the stated success criterion (§14).

**Cut:** Move §12.1 MA-credential-propagation from open question to Phase A work item. Kill A.1 (rename) to make room. Until gh-auth works, no recipe, no gate, no template matters.

---

## C2: Only 2 of 8 must-goals are directly evidenced by dogfood

**Must-goals (§3.1):**

| # | Goal | Evidence from dogfood? |
|---|------|----------------------|
| 1 | Multi-phase composability | No — user ran 2-team inline DAG, never needed phases |
| 2 | Reusable agent templates | No — zero recurring templates across the one run |
| 3 | Structured outputs | **Yes** — `result_summary` was the only data that survived; schema helps |
| 4 | Bounded conditional re-entry | No — no run looped; no gate was needed |
| 5 | Sub-workflows / nested invocation | No — no nested recipe was needed or attempted |
| 6 | Naming alignment (team→agent) | No — cosmetic; zero UX impact |
| 7 | Recipe registry + MCP surface | Partial — `dry_run` and discoverability are useful; full registry is speculative |
| 8 | Both backends supported | Existing — v2 already does this |

Goal 3 (artifacts) and the non-recipe half of Goal 7 (dry-run, cancel) are the only items directly motivated. Goals 1, 4, 5, 6 are vision work with no evidence trail.

---

## C3: 5-sprint phasing is padded — A + B are deferrable or combinable

**Phase A breakdown (§10, Phase A):** 6 items.
- A.1 is the rename. No user value. Burns ~30% of the sprint on aliases, parser changes, doc updates.
- A.2–A.4 are the actual dogfood fixes (LastError, LastTool, cancel). These are 3–5 PRs of real work independent of any v3 concept.
- A.5–A.6 are artifact storage — useful only when something writes artifacts, which requires Phase B/C.

**Phase B (templates)** ships an entire template loading + binding engine before any recipe exists to consume it. That's a feature with no live consumer for one full sprint.

**Realistic path:** A.2–A.4 ship as v2.x hotfixes this week. Artifact storage (A.5–A.6) merges into Phase C alongside the recipe runtime that actually needs it. Phase B collapses into Phase C. Sprint count: 3 (foundation fixes + recipe runtime + polish), not 5.

---

## C4: Sub-recipes (Phase E) should be cut entirely from v3

**§7.6, Phase E:**

The sub-recipe feature requires:
- Load-time inline expansion of nested recipes
- Cycle detection across all recipes
- Depth-cap enforcement (default 5)
- Cross-recipe artifact resolution (`artifacts.reviewer.*.review` wildcard)
- `instances: ${params.reviewers}` fan-out (§15.3) — a new concept not in the rest of the design

**Evidence for any of this:** zero. No user ever said "I need to nest a recipe inside a recipe." No dogfood run demonstrated fan-out. The motivating example (`parallel-review` as a sub-recipe) is equally expressible as a phase inside the parent recipe with explicit `reviewer-1`, `reviewer-2`, `reviewer-3` agent entries — ugly but functional, and no new engine.

**Cost of cutting:** Phase E is explicitly "~half sprint." Cutting it removes cycle detection, depth-cap enforcement, wildcard artifact resolution, and `instances` fan-out from scope. None of these appear in the §14 success criteria.

---

## C5: Gate mini-DSL (Phase D) is premature

**§7.5, §8.4:**

The expression language gets its own package (`internal/expr/`, ~300 LOC), a hand-rolled parser, a fixture suite, and fuzz testing — before any production recipe has ever looped once.

The design's own success criteria (§14) does not mention gates. The `feature-end-to-end` recipe in §15.2 shows a gate, but §15.1 (`single-feature-pr`) ships without one. The entire motivation is hypothetical: "if review rejects, loop back to engineer."

**YAGNI test:** can the Phase C recipe runtime ship with a degenerate gate that always passes (i.e., no re-entry)? Yes. Real recipe authors will hit the "need a loop" wall and file an issue with a concrete predicate. Design the DSL from those examples, not from first principles.

**Cut Phase D from v3.0.** Defer to v3.1 when a real recipe's loop requirement is documented. The `gate:` key can be a parse-time validation error ("gates not yet supported") until then.

---

## C6: Artifact storage is over-engineered for v3

**§7.2, §3.3.5, §8.5:**

The design introduces:
- A new directory layout (`.orchestra/artifacts/<agent>/<key>.*`)
- A `record_artifact` custom tool (sibling to `signal_completion`)
- JSON schema validation on artifact content
- MCP `get_artifacts` + `read_artifact` tools + 4 new resource URIs

**What exists today:** `signal_completion(summary)` writes to `state.json.teams[i].result_summary`. The dogfood run proved this is rich enough to carry a full implementation report. The per-team NDJSON already records every action.

**Minimal alternative:** extend `signal_completion` to accept an optional `artifacts: {key: value}` dict. Store as `state.json.teams[i].artifacts`. Surface in `RunView.Agents[i]`. This is ~30 lines of Go, no new directory, no new MCP tools beyond surfacing what's already in RunView. JSON schema validation and file-typed artifacts can ship when a recipe actually needs to pass a file path to a downstream agent — that's a Phase C problem, not Phase A.

The separate artifact storage system is YAGNI until at least one recipe with file-passing between agents exists in production.

---

## C7: Agent rename (§7.1, Phase A.1) is a distraction

The rename changes `team` → `agent` across CLI, MCP DTOs, internal Go types, docs, and YAML parser. The justification (§8.6) is "v3 is the breaking-change window" — circular. The user never asked for the rename. The dogfood run used `teams:` throughout with no confusion.

**Cost:** parser aliases, migration guide, v3.x deprecation warnings, v4.0 removal plan — all for a word swap. Every PR reviewer has to mentally track "does this file use the old or new name?" during the transition window.

**Cut A.1.** If the rename is genuinely important, do it as a standalone PR before v3 work starts, completely decoupled from the recipe runtime. Don't bundle cosmetic renaming into a sprint that should close the dogfood's P0 findings.

---

## C8: Phase F (docs/cookbook) is a hidden 20% tax, not free

**§10, Phase F:**

"5–10 worked examples," README rewrite, migration guide — billed as "~1 sprint." The design says docs are "free if the design is right." They are not free:
- 5–10 worked examples means 5–10 recipes need to be authored, validated, and tested against `make cookbook-test`.
- Each recipe is a design decision that may expose gaps in templates/artifacts/recipe schema.
- If the design is wrong, Phase F is when you discover it — after 4 sprints of implementation.

**Honest accounting:** Phase F is a user-acceptance test masquerading as documentation. It should precede Phase C (recipe runtime) as a design-doc-as-spec, not follow Phase E as cleanup. Write `feature-end-to-end.yaml` and `single-feature-pr.yaml` before writing `internal/recipes/` so the schema is driven by real examples.

---

## C9: §3.2 Should-have items — which are 12-month dead

| Item | Verdict |
|------|---------|
| Recipe versioning (§3.2.1) | Dead until recipes have real users who break on a version change |
| Artifact lineage (§3.2.2) | Dead — path + produced_at already covers 95% of the use case |
| Dry-run / planning mode (§3.2.3) | **Ship in Phase C** — motivated by feedback doc §6, not a should-have |
| Per-recipe defaults (§3.2.4) | Dead — YAML default values cover this without a new override mechanism |

Promote `dry_run` to must-have (Phase C). Delete the other three from scope.

---

## C10: The design hides the actual blocker behind feature work

**The real v3 user story from dogfood:** "I ran orchestra from MCP, both agents died on `gh auth status`, no PR shipped."

**What v3 ships in Phase A:** `team→agent` rename, LastError, LastTool, cancel, artifact storage — all real improvements, but the rename is noise and artifact storage is speculative.

**What v3 defers:** gh-auth credential propagation (§12.1) — the one thing that made every real recipe fail.

The composable-workflow vision is coherent, but the phasing shields the recipe machinery from the evidence. A user cannot benefit from `feature-end-to-end.yaml` until MA sandboxes can push a branch. The design should invert the order: ship gh-auth fix → ship `single-feature-pr` recipe (Phase C, no phases, no gates, no sub-recipes) → validate it works → then consider whether re-entry and sub-recipes are needed.

**Proposed minimal v3 scope:**
1. v2.x: LastError, LastTool, Tokens, cancel, liveness check. (All from feedback-mcp-server.md top-7.)
2. v3.0-A: gh-auth credential propagation into MA sandboxes.
3. v3.0-B: `signal_completion` artifact extension (key/value, no separate storage system).
4. v3.0-C: Recipe runtime (no gates, no sub-recipes) + `single-feature-pr` as first recipe.
5. v3.1: Gates + re-entry (when a real recipe needs a loop).
6. v3.2: Sub-recipes (when a real recipe needs nesting).

This is 2 real sprints, not 5, and it starts from the evidence.
