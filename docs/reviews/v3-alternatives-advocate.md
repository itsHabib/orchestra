# v3 Alternatives Advocate: Principled Counter-Proposals

Reviewer: alternatives-advocate
Date: 2026-05-02
Docs reviewed: `docs/feedback-mcp-server.md` plus an **earlier draft** of `docs/DESIGN-v3-composable-workflows.md`.

> Status note: this counter-proposal was written against the pre-revision
> design draft. The published design simplified several of the structures
> this critique pushed back on — gate expressions are now hardcoded
> `signal_status == X` predicates rather than a `${outputs...}` DSL,
> artifacts ride signal-event payloads (no separate storage layer), and
> sub-recipes were removed from v3.0. The doc is preserved unmodified so
> the design-revision audit trail stays intact; references to
> `${outputs...}`, `record_artifact`, the `messages/` bus, and inline-expansion
> sub-recipes describe the pre-revision shape, not the binding design.

---

## C1: YAML + Go template engine is the worst of both worlds (§7.3, §8.1)

The design justifies YAML by "LLM-authorability" and "hot-reload" (§8.1). The actual audience is Go developers. YAML buys nothing a Go struct literal doesn't, and loses compile-time type checking, IDE completion, and `go vet`.

More damaging: the template system embeds Go's `text/template` syntax inside YAML:

```yaml
prompt: |
  Rubric:
  {{ readFile .params.rubric }}

  Input:
  {{ .inputs.draft }}
```

This is Go template errors inside YAML validation stacks. Not type-safe like Go, not schema-validated like JSON Schema, invisible to YAML linters. A typo in `{{ readFile .params.rubuc }}` surfaces at run time, 10 minutes in, with a Go template error message inside a yaml.Unmarshal stacktrace.

**Counter-proposal:** Templates as Go functions with a builder API—`templates.New("critic").Role("critic").FileParam("rubric").PromptFn(func(p Params) string {...})`. ~20 lines. Compiler validates. IDEs complete. YAML is an export format for sharing, not the authorship surface. The LLM can write Go.

---

## C2: Server-side gate evaluation precludes real review use cases (§7.5)

The design requires every gate to be a deterministic equality/comparison check over a structured JSON artifact:

```
${outputs.review_decision.decision} == 'approved'
```

This is only sound if reviewer agents reliably emit `{decision: "approved"}`. In practice: the reviewer is an LLM. "I have mixed feelings, there are concerns but also strengths" does not map cleanly to `approved|revise`. The design silently pushes all qualitative judgment into the agent's output schema—with no fallback when the agent hedges or the JSON is malformed.

**Honest cost/correctness comparison:**

| Gate type | Latency | Cost | Works for |
|---|---|---|---|
| Server-side expression (chosen) | ~0ms | $0 | Binary artifact fields (`decision == proceed`) |
| Reviewer-agent gate (alternative) | 2-5s | ~$0.002 | Qualitative review, multi-criteria, natural language reasoning |

The design mentions `${outputs.tests.passed} && ${outputs.tests.coverage} > 0.8` as an example. That's a CI gate, not a review gate. The motivating use case ("is this design ready to implement?") is qualitative and cannot be reduced to a JSON boolean without an LLM making that reduction—which is the gate.

**Counter-proposal:** Add `gate.kind: llm_eval` that calls a lightweight model (haiku-class) once with the gate artifacts and a rubric. 2-5s, sub-cent cost per phase transition, unlocks the actual review-and-iterate cycle the design describes. Server-side remains default for deterministic cases.

---

## C3: Inline sub-recipe expansion breaks observability at the structural boundary (§7.6, §8.3)

The design argues inline expansion "keeps run state coherent (one `state.json`, one log dir, one MCP record)." But this trades structural coherence for observational opacity:

- A recipe with 3 nested sub-recipes × 4 agents each = a flat 12-agent DAG in `get_run`. No grouping by logical sub-recipe.
- `AgentView.Phase` (§7.7.3) is a recipe-level concept. Sub-recipes flattened into the parent have no phase boundary visible to `get_run`.
- `cancel_run` cancels everything. No "cancel this sub-recipe, return what it produced so far."
- The inline-expansion depth cap of 5 (§7.6) is a silent complexity bomb: 5 levels × 4 agents each = 1024 nodes in the worst case.

The design's stated reason to reject separate runs—"fragments the chat-side view"—is solved more cheaply by a `get_run` that includes child run IDs with their status.

**Counter-proposal:** `wait_for_run` primitive. Sub-recipe invocation is a real `orchestra run`. Parent agent (or the engine itself) calls `wait_for_run(child_run_id)` and receives the sub-recipe's artifact outputs. Each component run has its own `get_run`, its own `cancel_run`, and its own log dir. Chat-side LLM can inspect each independently. Cross-run artifact refs are simple: `read_artifact(run_id=child_id, agent=..., key=...)`. Cost: N `state.json` files. Payoff: clean observability per composition boundary.

---

## C4: Phases as first-class when deps + signal_required suffices (§7.4, §8.2)

§7.4 admits: "Phases are *not* a separate execution primitive. Internally, a recipe expands into a flat DAG with synthetic gate-nodes." This is a clear signal the concept is paying overhead cost without adding execution power.

What phases add over plain deps:
- A new syntactic concept (user learns `phases:` + `agents:` + `gate:` instead of just `agents:` + `deps:`)
- Re-entry semantics that retry the *entire phase*—if 1 of 4 design agents fails, all 4 re-run
- An explicit `gate` concept only usable at phase boundaries

What deps + a `signal_required` flag on specific agents gives you:
- `engineer` agent has `deps: [critic, design_reviewer]`—waits for both naturally
- `design_reviewer` signals a structured completion with `{decision: proceed|revise, rationale: ...}`
- Engineer's inputs already include the reviewer's artifact, which contains the rejection rationale
- Retry unit is the individual failing agent—no coarse-grained phase re-run

The only capability phases add that deps don't express is "name a boundary for the gate." But the gate boundary *is* the downstream dep. Naming it `phase: design` is sugar that costs a concept.

**Counter-proposal:** Drop phases; keep flat deps. Add `max_retries` per agent. Gate evaluation is a predicate on an upstream agent's artifact, evaluated when the downstream agent becomes ready to schedule. One concept (agents with deps) instead of three (agents, phases, gates).

---

## C5: Artifacts as a new storage layer when the message bus would cover it (§7.2, §7.8, §8.5)

The design adds `.orchestra/artifacts/<agent>/<key>` alongside `.orchestra/messages/` and `.orchestra/results/`. This is three places for agent-to-agent communication:

| Layer | Purpose | Location |
|---|---|---|
| `results/` | `signal_completion` summary | `.orchestra/results/<agent>.json` |
| `messages/` | steering, inter-agent comms | `.orchestra/messages/<inbox>/inbox/` |
| `artifacts/` | structured outputs (NEW) | `.orchestra/artifacts/<agent>/<key>` |

The message bus already does atomic writes, key-based addressing, and cross-agent consumption. The missing pieces are typed schema and artifact declaration. Both can be added as a message shape:

```json
{"type": "artifact", "key": "design_doc", "content_type": "file", "path": "...", "schema": {...}}
```

`record_artifact(key="design_doc", ...)` becomes `send_message(recipient="*", type="artifact", ...)`. `get_artifacts` becomes a filter on `read_messages(type="artifact")`. No new package, no new workspace directory, no new MCP tool pair.

**Concrete cost of the new layer:** `internal/artifacts/`, `mcp/artifacts_tool.go`, `get_artifacts` tool, `read_artifact` tool, two new resource URIs, storage layout changes to `state.json`. All avoidable with a typed message shape.

---

## C6: What if there is no recipe layer? (§2, §6, §10)

The design doesn't honestly evaluate the null alternative: **ship zero recipe primitives in v3** and instead:

1. Phase A observability improvements (`LastError`, `LastTool`, `LastEventAt`, `Tokens`)—already planned
2. `cancel_run`—already planned
3. `record_artifact` + `read_artifact`—already planned
4. `wait_for_run` as the sole composition primitive (1 sprint, new work)

With these four, the chat-side LLM can implement design → impl → review as a multi-turn conversation:
1. `run(inline_dag={design agents})`
2. Poll until done, `read_artifact(design_doc)`
3. `run(inline_dag={impl agents, inputs.design=...})`
4. Repeat for review phase, gate on artifact field

This isn't a strawman—the dogfood run drove a real two-team workflow this way. The coordinator LLM was the composer. Recipes add **configuration-time composition** (author once, run many times with different params). For a 1-2 person project running bespoke features, that reuse may not materialize for many months.

**The honest recipe-layer cost**: 5 sprints × 1 week = ~10 weeks of Go engineering. New packages: `internal/recipes/`, `internal/templates/`, `internal/artifacts/`, `internal/expr/`, `cmd/recipe.go`, `cmd/template.go`, `internal/run/recipe_lifecycle.go`, 3 new MCP tool handlers, 7 new resources. Payoff: parameterized reuse across runs.

**Recommendation**: Ship Phase A (observability + artifacts + cancel) as "v3-foundation." Defer recipe/template/phase machinery until there are ≥3 concrete "I want to run this recipe 10 times" use cases from real dogfood. The `inline_dag` model may be the right steady state indefinitely.

---

## C7: MCP resources in §7.7.2 should be subscriptions, not snapshots (§7.7.2, feedback-mcp-server.md §Coordinator UX)

The dogfood feedback doc explicitly names this: "push notifications. If MCP supports it (it does, via resource subscriptions), surface team-status-change events so the coordinator can sleep until something actually changes. Cuts wake-ups by ~90%." The v3 design acknowledges this in the feedback doc but does not include subscription semantics in the §7.7.2 resource table.

Every resource listed in §7.7.2 is a read-only snapshot:
- `orchestra://runs/{id}` → read snapshot
- `orchestra://runs/{id}/artifacts/{agent}/{key}` → read snapshot
- `orchestra://recipes/{name}` → read snapshot

None are listed with subscription semantics. The coordinator pattern (poll at 270s, wake on event) is the highest-leverage UX improvement available—and it requires exactly one change: `orchestra://runs/{id}` supports `resources/subscribe`. When a team status changes, the MCP server pushes a notification. The coordinator sleeps indefinitely between changes.

**Concrete ask**: Add explicit subscription support to `orchestra://runs/{id}` and `orchestra://runs/{id}/artifacts/{agent}/{key}` (fires when a new artifact is written). Tag both as `subscribable: true` in the resource schema. This is 1-2 days of MCP server work (SSE upgrade on the resource endpoint). It should be Phase A, before recipes—every coordinator pattern benefits.

---

## C8: No recipe authoring scaffold despite "LLM is the composer" claim (§7.9, §10 Phase C)

The design plans `orchestra recipe validate/run/list/show` (§7.9) but no `scaffold` or `init`. Creating a recipe requires knowing:
- Valid YAML field names (`agents:` not `teams:`, `deps:` not `depends_on:`, `gate:` not `condition:`)
- Correct interpolation syntax (`${artifacts.X.Y}` not `{{.artifacts.X.Y}}` or `$artifacts.X.Y`)
- Available templates and their exact parameter names
- Phase/gate schema

The LLM will get these wrong. It will hallucinate field names and produce a recipe that fails `orchestra recipe validate` with a Go YAML unmarshaling error referencing an internal struct field name, not the recipe field name.

`orchestra recipe scaffold --name my-recipe --phases design,impl --templates engineer@1,reviewer@1` emitting a valid skeleton with placeholder comments takes ~3 hours to write. The payoff: the LLM's first generated recipe passes `validate` on first try instead of third. This is the difference between "LLM can author recipes" (the stated goal) and "LLM eventually authors recipes after debugging YAML schema errors."

**Recommendation**: Add `scaffold` to Phase C's CLI surface. It costs one afternoon and is the linchpin of the "LLM as composer" thesis.

---

## Strongest counter-proposal

**Ship nothing but Phase A + `wait_for_run`**, then stop and dogfood for 4 weeks. Phase A is 6 concrete observability improvements that address every top-10 item from the feedback doc. `wait_for_run` is the minimal composition primitive that lets the chat-side LLM chain runs without a recipe engine. The recipe/template/phase/expression-parser machinery (5 sprints, ~10 weeks) should be gated on evidence that bespoke `inline_dag` composition is actually the bottleneck—not on the thesis that "reusable recipes will be valuable." The dogfood evidence so far shows the bottleneck is observability (dead subprocesses, opaque `get_run`), not recipe reuse. Fix the known bottleneck first; build the recipe layer only when the reuse bottleneck is observed.
