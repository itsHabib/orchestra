# Architecture Critique: orchestra v3 Composable Workflows

Reviewer: architecture-critic
Date: 2026-05-02
Docs reviewed: an **earlier draft** of `docs/DESIGN-v3-composable-workflows.md` plus `docs/feedback-mcp-server.md`.

> Status note: this critique was written against the pre-revision design draft.
> Several of the issues below were resolved in the published design — most
> notably C2 (artifact filesystem on MA), where the design now persists
> signal-event artifacts host-side regardless of backend, and C3/C8 where
> the gate DSL was simplified to hardcoded `signal_status` predicates and
> the recipe runtime was deferred to Phase B. The doc is preserved unmodified
> so the design-revision audit trail stays intact; treat references to
> `record_artifact`, `${...}` interpolation, and the inline-expansion sub-recipe
> mechanism as referring to the pre-revision shape, not the binding design.

---

## C1 — Phases claim to be compile-time sugar but are a runtime primitive (§7.4, §7.5, §8.2)

§8.2 states: *"Phases are not a separate execution primitive. Internally, a recipe expands into a flat DAG with synthetic gate-nodes between phases."*

This is self-contradicted by the design's own details:

- `state.json` grows a `phase_iters` map keyed by phase name (§7.7.3 `RunView.PhaseIters`). The engine must read and write this map at runtime — phases have identity after expansion.
- Re-entry semantics say "re-execute the named phase" (§7.5). The engine must know which nodes belong to which phase at re-entry time — this is not recoverable from a flat DAG without out-of-band metadata.
- `RunView.Phase` surfaces the current phase name to the MCP layer (§7.7.3). The runtime tracks phase position as a first-class state machine field.
- `${phases.design.outputs.design_doc}` (§7.4) is a recipe-level reference that requires the engine to maintain a phase → output mapping beyond the flat node graph.

**The claim is false.** Phases are a runtime concept. Calling them "sugar" defers hard questions: Does the engine re-expand YAML to know phase membership after a re-entry, or does it store a phase→node index in state.json? What happens to in-flight artifacts from a failed phase iteration — are they overwritten or versioned? The flat-DAG framing obscures these questions rather than answering them.

**Concrete risk:** the gate evaluator in `internal/run/recipe_lifecycle.go` needs to know, for each gate-node, which upstream nodes to re-execute and which to skip. With a truly flat DAG, synthetic gate-nodes have no "belongs to phase X" edge — you'd need a side-index. The design never specifies this index.

**Recommendation:** Acknowledge phases as a first-class runtime concept with a defined state machine (pending → running → gated → re-entering → done). Drop the "not a separate primitive" claim. The alternative (truly flattening to a labeled DAG) is valid but requires a different gate/re-entry design than the one described.

---

## C2 — Artifact filesystem writes are incompatible with MA backend (§7.2, §3.1 "both backends supported")

§7.2 specifies that `record_artifact` writes to `.orchestra/artifacts/<agent>/<key>.{ext}` — a path in the local run workspace on the machine running the orchestra process.

MA agents run inside Anthropic-managed sandboxes with no access to the local filesystem. The feedback doc (dogfood run §) confirms MA sandboxes start with no GH credentials and no pre-installed tools — they're isolated compute environments. An MA agent calling `record_artifact(key="design_doc", path="docs/feature-X.md")` has no path to write to that resolves to the orchestra server's `.orchestra/artifacts/` directory.

For `signal_completion`, this works because it's a custom tool that makes an outbound API call to the orchestra MCP server — the tool call returns a value that orchestra receives over the MA event stream. `record_artifact` must work the same way: the agent calls it, the call goes over the event stream to orchestra's MA session handler, which then writes the content to the local workspace. But:

1. The design never specifies `record_artifact` as an MCP/API-bridged tool for MA. §7.2 describes it as a filesystem operation: "Orchestra validates the key against the template's declared outputs and the json against any provided schema."
2. For a `type: file` artifact (the most common case — `design_doc`, `pr_url`), the agent references a file *inside its sandbox*. How does orchestra retrieve that content? `signal_completion` carries a string summary. `record_artifact` with `type: file` would need to carry the full file content over the event stream — potentially megabytes.
3. The `record_artifact(key="design_doc", path="docs/feature-X.md")` API takes a path, not content. For local backend, orchestra reads the file from the path. For MA, there's no file at that path on the orchestra host. The abstraction breaks at the parameter level.

§3.1 lists "Both backends supported" as a must-have functional goal. This goal is not met by the current artifact design.

**Recommendation:** Define `record_artifact` as a custom tool with two implementations: local (filesystem write) and MA (content upload over the session event stream). For `type: file`, require the MA agent to emit content inline (or via a temporary URL). This is a significant design gap, not a minor implementation detail — it affects the wire format, the agent API, and the storage model.

---

## C3 — Gate expressions and recipe interpolation share `${}` syntax with incompatible semantics (§7.4, §7.5, §8.4)

The design uses `${}` for two structurally different operations:

**Interpolation at recipe-load time** (parameter binding, artifact path references):
```yaml
params: {rubric: "${params.rubric}"}
inputs: {design: "${artifacts.designer.design_doc}"}
```
These resolve when the recipe is loaded and agents are instantiated.

**Gate expressions evaluated after agents complete**:
```yaml
gate:
  condition: "${outputs.review_decision.decision} == 'approved'"
```
This resolves at gate-evaluation time, after the phase's agents have run and written artifacts.

These are two separate evaluation passes. The `${...}` sigil looks identical to a recipe author. A mistaken recipe like:

```yaml
gate:
  condition: "${params.threshold} < ${outputs.score.value}"
```

...uses `params.threshold` (a load-time binding, valid) mixed with `outputs.score.value` (a runtime binding). This is probably valid in the gate DSL, but what if someone writes:

```yaml
gate:
  condition: "${artifacts.reviewer.critique.approved} == true"
```

Is this the gate DSL's artifact access, or the recipe-level interpolation syntax? Are the two `${}` systems sharing a resolver, or are they independent? The design doesn't specify.

§8.4 justifies the custom DSL vs. CEL on the grounds of simplicity and auditability, but the "two namespaces, one sigil" problem exists regardless of which DSL is chosen. The expression evaluator in `internal/expr/` (§7.9) presumably handles *only* gate-expression syntax. Recipe-level interpolation presumably happens in `internal/templates/` or `internal/recipes/`. If a gate condition string first passes through the recipe interpolation pass and *then* through the expression evaluator, `${params.threshold}` would be substituted to a literal value before the expression engine sees it — which might be the intended behavior, but it creates a two-pass evaluation order that must be documented and tested explicitly.

**This is a two-languages problem.** The expression language is separate from the parameter-binding language and both use `${}`. §8.4 defends the DSL choice but does not address this. The alternative — structural gate conditions (`{ lhs: "outputs.review_decision.decision", op: "eq", rhs: "approved" }`) — eliminates the ambiguity entirely at the cost of expressiveness.

---

## C4 — Sub-recipe `instances: "${params.reviewers}"` is irreconcilable with load-time inline expansion (§7.6, §15.3)

§7.6 states: *"At recipe-load time, `parallel-review@2` is loaded and inline-expanded."*

§15.3 introduces:
```yaml
- name: reviewer
  template: pr-reviewer@1
  instances: "${params.reviewers}"
```

Where `reviewers` is a recipe parameter (`type: int, default: 3`).

These two statements are mutually exclusive. If inline expansion happens at load time (before `run_recipe` is called), the `instances` value is a `${}` expression, not a resolved integer. The expander cannot emit N reviewer nodes when it doesn't know N.

The design never resolves this. Either:
- (a) `instances` is resolved at load time by requiring all parameters that affect fanout to be provided at load time (before spawning). This means `list_recipes` and `get_recipe` cannot show the concrete agent count without parameters — the recipe is polymorphic in shape, not just in content.
- (b) DAG construction is deferred to run time (when parameters are known). This contradicts §7.6's "load time" claim and means the DAG visible in `dry_run` output is a template, not a concrete graph.
- (c) `instances` is not part of the inline-expansion mechanism — the spawner handles fan-out as a dynamic runtime operation over the flat DAG. But then it's not "inline expansion at load time."

**Impact is large:** if an engineer implements §7.6 as written (load-time expansion) and then later tries to add §15.3's `instances` feature, they'll discover the expansion model is wrong and need to redesign it. This should be resolved in the design doc before Phase E.

---

## C5 — Missing primitive: phase output schema at phase boundaries (§7.2, §7.4)

Agent templates declare output schemas (§7.2):
```yaml
outputs:
  decision_summary:
    type: json
    schema: { required: [decision, rationale], ... }
```

Phase declarations map output names to artifact paths (§7.4):
```yaml
phases:
  design:
    outputs:
      approved_design: "${artifacts.design_reviewer.decision_summary}"
      design_doc: "${artifacts.designer.design_doc}"
```

The downstream phase references phase outputs:
```yaml
inputs: {design: "${phases.design.outputs.design_doc}"}
```

**Nowhere is there a schema declaration at the phase-output level.** The `phases.design.outputs.design_doc` mapping resolves to `${artifacts.designer.design_doc}`, which is a `type: file` artifact. If the consuming agent's template declares `inputs: { design: { type: json } }`, the type mismatch is caught at agent instantiation — but only at agent instantiation, which happens at run time, deep into the recipe execution. For a 3-phase recipe, a type mismatch between phase 1 and phase 3 surfaces as a runtime failure hours into the run.

The design could define phase-level input/output schemas that are validated at recipe-load time (same static analysis pass that does cycle detection). This would catch type mismatches before any agent runs. The current design forces errors to be late and expensive.

The gate expression `${outputs.review_decision.decision}` is also untyped at the recipe-validation level — if `review_decision` is a `type: file` artifact (markdown), the `.decision` field access fails at gate-eval time with a runtime error, not a validation error.

**One sentence:** the design has agent-level artifact schemas and phase-level output mappings, but no phase-level contract layer that can be validated statically.

---

## C6 — `run_recipe` resolution scope is unspecified, likely forcing business logic into the MCP layer (§7.7, §7.9, §7.7.3)

§7.7.3 states: *"Internal types in `internal/recipes/`, `internal/templates/`, `internal/artifacts/` are not exposed directly. MCP is an adapter, not a window into internals."*

The `run_recipe` MCP handler (`internal/mcp/recipes_tool.go`, §7.9) must perform:
1. Load recipe from registry (`internal/recipes/`)
2. Bind parameters (`internal/templates/`)
3. Expand sub-recipes inline (`internal/recipes/` + cycle detection)
4. Build the agent DAG
5. Validate artifact refs across phases
6. Call into the spawner (or `internal/run/`)

If step 6 is the only call to an internal service and steps 1–5 happen inside `recipes_tool.go`, the MCP tool handler is doing recipe orchestration — that is business logic, not adaptation. The layering rule is violated.

The design does not specify a service layer API that `recipes_tool.go` would call. The closest is `internal/run/recipe_lifecycle.go` (phase transitions, gate eval) and `internal/recipes/` (loading). Without an explicit `RecipeService.Run(name, params, opts) (RunID, error)` or equivalent, implementers will inline the orchestration in the MCP handler because that's the path of least resistance.

The same concern applies to `dry_run`: returning the resolved DAG requires executing steps 1–5 without step 6. If the MCP handler owns the `if dry_run { return dag } else { spawn(dag) }` branch, that's control flow that belongs in the service layer.

**Recommendation:** Before Phase C, define the internal service APIs (`RecipeService.Resolve`, `RecipeService.Run`, `RecipeService.DryRun`) that the MCP handler calls. The handler should be ≤50 lines: deserialize, call service, serialize, return.

---

## C7 — Agent template output contracts are advisory, not enforced; failure surfaces to the wrong party (§7.3, §7.2)

The template declares `outputs:`:
```yaml
outputs:
  critique: {type: json, schema: {...}}
```

The agent fulfills it by calling:
```
record_artifact(key="critique", json={...})
```

Orchestra validates the key and schema on `record_artifact` call — a call that happens inside the agent's session. But if the agent *never calls `record_artifact`* (because it decided it was done, or because it went off-script, or because it hit a timeout), no validation error fires. The downstream agent that depends on `${artifacts.critic.critique}` receives either a nil artifact or a hard failure at prompt-injection time — an error that surfaces to the recipe runtime, not to the errant agent.

The design says (§7.2): *"Mismatch = clear error to the agent."* But that error only fires if the agent calls `record_artifact` with wrong content. Silent non-production — the most common failure mode (agent does work, doesn't formally declare outputs, calls `signal_completion(done)`) — produces no error to the agent. It produces a late, cryptic failure in the next phase.

This is a fundamental asymmetry: the template contract is checked on the *call that fulfills* the contract, but fulfillment is optional from the agent's perspective. A genuine contract would require the spawner to inject a "you MUST call record_artifact(key=X) before signal_completion" obligation and enforce it at session end — and fail the agent (not the next phase) if not met.

**Risk in practice:** the dogfood run showed `ma-dispatch` calling `signal_completion(done)` despite not having shipped a PR — the "done" signal was misleading. With artifact contracts, the same pattern produces: agent signals done, recipe tries to read `pr_url` artifact, artifact doesn't exist, phase 3 fails cryptically. The design hasn't eliminated this failure mode; it's moved it one step later and made it harder to attribute.

---

## C8 — Re-entry injection growth is unbounded and invalidates the prompt cache (§7.5)

§7.5 re-entry semantics, point 3: *"On re-entry, the agents see the previous phase's gate failure as an additional input (so the engineer knows what review feedback to address)."*

On iteration N, the engineer receives: original inputs + gate-failure-from-iteration-1 + gate-failure-from-iteration-2 + ... + gate-failure-from-iteration-N-1.

The design does not:
- Bound the size of a gate-failure injection
- Specify the format ("previous gate failure" is an artifact? a string? a summary?)
- Describe how these accumulate in the prompt

At `max_iters: 3`, iteration 3's prompt has two injected gate failures. Each gate failure may include the full artifact that failed evaluation (the `critique.json` or `review_decision.json`). For real recipes, these artifacts can be thousands of tokens. The engineer's prompt grows with each iteration, burning prompt cache (the injection changes per iteration, so cache hits on the system prompt are unaffected, but the variable portion grows).

More subtly: if the "gate failure injection" is prepended to or appended to the task prompt in a different position each iteration, the cache key changes and the expensive part of the prompt (system + tools) must be re-cached. The feedback doc noted cache_read_input_tokens of 3.3M per run — that's the asset being burned here.

**The design has no spec for this injection**, which means implementers will choose arbitrarily, leading to recipes that behave unexpectedly when they actually loop. This should be specified concretely before Phase D ships.

---

## Summary of concerns by phase impact

| # | Concern | Blocks |
|---|---------|--------|
| C1 | Phases are an undeclared runtime primitive | Phase C, Phase D |
| C2 | Artifacts incompatible with MA backend | Phase A (record_artifact), Phase C |
| C3 | `${}` two-namespace collision | Phase C (gate authoring) |
| C4 | `instances` parameter irreconcilable with load-time expansion | Phase E |
| C5 | No phase-boundary schema layer | Phase C (static validation) |
| C6 | `run_recipe` resolution scope forces business logic into MCP | Phase C |
| C7 | Template output contracts are advisory only | Phase B, Phase C |
| C8 | Re-entry injection growth unbounded | Phase D |

**Single strongest concern (C2):** The artifact system assumes local filesystem access, but MA agents run in isolated remote sandboxes. `§3.1` promises "Both backends supported" as a binding must-have functional goal. Without defining how `record_artifact` works over the MA event stream (content transport, large-file handling, path semantics), the entire composable workflow vision — phases passing structured artifacts to downstream phases — is valid only for the local backend. Every Phase B–E feature built on top of artifacts inherits this breakage. This is not an implementation detail; it's a wire-format and API design decision that must be resolved before Phase A ships `record_artifact`.
