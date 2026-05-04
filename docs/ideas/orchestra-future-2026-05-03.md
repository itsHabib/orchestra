# Orchestra: Top 3 Future Ideas (post Phase A)

Generated: 2026-05-03 by orchestra dogfood run

## Context

This brainstorm ran on 2026-05-03 as an orchestra dogfood exercise immediately after Phase A merged. Three parallel designers worked independently from different angles (user-UX, runtime/substrate, ecosystem/integration), then a reviewer critiqued all three lists. This synthesis picks the top 3 across the full corpus of 15 ideas. Scope constraints: ideas already committed in Phase A (§3 baselines) are excluded, as are §13 non-goals (hosted catalog/marketplace, GUI canvas builder, cross-org authority, persistent server processes). The v3 design's explicit exclusions around Phase A scope (credential injection, inline DAG, run_recipe, basic MCP tools) are treated as done and not re-ranked here.

---

## Idea 1: Non-Retriable Error Fast-Fail

- **Why it's #1:** The dogfood run documented a concrete 4-attempt burn on errors (`billing_error`, `authentication_error`) that the MA SDK already classifies as permanent — orchestra just isn't acting on that classification. This is the single highest-ROI change in the entire corpus: ~30 lines of Go, zero schema changes, and it eliminates a failure mode that visibly wasted attempts and obscured the real error message in the final `AgentView.LastError`.
- **Concrete shape:**
  - Add a two-bucket classifier in `internal/spawner/` that maps MA SDK `error.type` values to `Retriable` vs. `Permanent`.
  - On `Permanent`, skip remaining retry attempts and surface the full error message immediately in `AgentView.LastError`.
  - Add a test fixture with a `billing_error` response to lock in the behavior.
  - Emit a structured log event (`error_type`, `permanent: true`, `retries_skipped`) for downstream observability.
  - No changes to recipe schema, state.json structure, or MCP tool signatures.
- **Effort/impact:** Effort: Small (~30–50 lines of Go, 1–2 days). Impact: High — directly eliminates a documented multi-attempt waste cycle; makes error messages actionable on first failure.
- **Risks:**
  - MA SDK `error.type` taxonomy may change across versions; the classifier needs to fail-open (treat unknown types as retriable) to avoid blocking valid operations.
  - Over-classification of permanent errors could suppress legitimate retries on transient SDK bugs — needs careful initial enumeration.

---

## Idea 2: OpenTelemetry Trace & Metrics Export

- **Why it's #2:** Four of the fifteen ideas across all three designers were observability-flavored, which signals a real gap. Rather than narrow MCP log-resource shims, OTEL is the right long-term foundation: it exports to every monitoring stack (Honeycomb, Datadog, Grafana/Tempo, Jaeger) with one config line, the Go SDK is mature, and it subsumes the narrower "NDJSON as MCP resource" and "signal_summary surfacing" proposals. The reviewer rightly called this out as the convergence point for all observability work. Budget caps (Substrate #4) can share the same token-counting infrastructure, making both features cheaper together than separately.
- **Concrete shape:**
  - Add `{"telemetry": {"otlp_endpoint": "...", "service_name": "orchestra"}}` to `config.json`; fully opt-in, no phone-home default.
  - Emit a root span per `run_recipe` with child spans per phase and per agent; span attributes carry `recipe.name`, `agent.name`, `signal_status`, `last_error`.
  - Export metrics: `orchestra.agent.tokens.{input,output,cache_read}` counters, `orchestra.recipe.duration_seconds` histogram, `orchestra.phase.iters` gauge.
  - Instrument the spawner event loop to emit spans alongside existing NDJSON writes — no removal of existing logging.
  - Gate behind the `telemetry` config block so zero behavior change for users who don't configure it; add an integration test that verifies span emission against an in-process OTEL collector.
- **Effort/impact:** Effort: Medium (OTEL Go SDK instrumentation of spawner + recipe lifecycle, ~1–2 engineer-weeks). Impact: High — enables cost attribution, SLO alerting, and phase-level perf debugging without NDJSON filesystem access; shared token infrastructure feeds Run Budget Caps for free.
- **Risks:**
  - OTEL adds a non-trivial dependency surface; needs vendor pinning and a clear upgrade policy.
  - Span cardinality from large runs (many agents × many phases) could be expensive in hosted backends — need per-agent sampling or aggregation guidance in docs.

---

## Idea 3: Recipe Taps — Git-Native Recipe Distribution

- **Why it's #3:** This is the only idea across all 15 that directly addresses cross-project recipe sharing, and it does so perfectly within §13's explicit constraint ("no hosted catalog; git is the alternative"). A Homebrew-style taps model makes git the distribution mechanism with zero new infrastructure — no catalog service, no hosted store, no cross-org authority. The reviewer ranked it highly, and there is no competing idea for this problem space.
- **Concrete shape:**
  - `orchestra recipe tap add <org/repo>` — registers a tap in `~/.config/orchestra/taps.json`; `tap update` runs `git fetch`.
  - `orchestra recipe pull <name> --from <org/repo>@<tag>` — resolves and copies a recipe/template into the local project.
  - A tap repo is a plain git repo with `recipes/` and `templates/` in the existing v3 YAML layout; versioning uses git tags.
  - Resolution precedence: project-local → user-global → registered taps (first match wins); conflict is surfaced, not silently resolved.
  - `orchestra recipe tap list` shows registered taps, last-fetched commit, and available recipe names.
- **Effort/impact:** Effort: Low-Medium (tap registry is a JSON file + git-fetch wrapper + one additional lookup in existing recipe resolution, ~1 engineer-week). Impact: High — unlocks org-scale recipe reuse, answers the "how do I share recipes across projects" question definitively, requires no new server infrastructure.
- **Risks:**
  - Tap versioning relies on git tag discipline in tap repos; a broken or force-pushed tag can break consumers — docs should recommend immutable tags.
  - Precedence rules (local > global > taps) may surprise users when a project-local recipe silently shadows a tap recipe of the same name; explicit warning on shadow needed.

---

## Honorable mentions

- **MA Cache Stale-ID Auto-Recovery + `force_recreate` flag (Substrate #2):** Hit 3 times in one dogfood session after key rotation; fix is local to 2 files. Nearly made the top 3 — just edged out by OTEL's broader payoff.
- **Run Budget Caps (Substrate #4):** The `max_iters: 3` silent-cost-tripling observation is sharp and 80% of the token instrumentation already exists; should ship alongside OTEL since they share infrastructure.
- **Inline DAG Prompt Preview with `preview_only: true` (UX #3):** Catches prompt typos in 2s instead of 10 min with small effort; a strong quick win for developer experience once the core reliability items (Ideas 1–2) are stable.
- **Run Checkpoint/Resume (Reviewer-proposed):** No designer addressed restarting a failed 6-phase run from phase 4; the data is in `state.json` and the missing piece is re-entry logic. Worth a dedicated spike.

---

## Process notes

The substrate designer produced the most empirically grounded ideas — every substrate proposal cited a specific observed failure mode with a line count and file location. That made the cost/benefit ratio immediately legible and is why two of the three top picks originated from that angle. The ecosystem designer's OTEL and Taps ideas were strong precisely because they proposed well-scoped primitives rather than full product features. The UX designer's ideas were consistently valid but often overlapped with substrate ideas or were half-implementations of ideas that needed a substrate change to land properly (e.g., push-model observability requires subscription support the compatibility layer was meant to paper over). The reviewer's most valuable contribution was identifying the observability cluster (4/15 ideas) as a convergence point and naming OTEL as the right unifying foundation — that cross-list synthesis is exactly the kind of signal a future brainstorm should use a reviewer role to surface. Meta-finding: explicit "cite a specific observed failure" guidance for designers would further tighten idea quality in future rounds.
