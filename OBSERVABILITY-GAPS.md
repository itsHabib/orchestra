# Observability & Self-Correction Gaps

Discovered during the Fusion AI Hackathon (2026-03-09). Orchestra completed all 9 teams but 3 silently failed to produce deliverables. We had zero signal until manually inspecting output.

## What Went Wrong

1. **`build-jobs` finished in 10 seconds** — process exited 0, orchestra marked it "done". No check that 10s is absurdly fast for a build phase. No check that deliverables exist.
2. **Domain judging produced 1 of 3 scorecards** — team completed "successfully" but 2 judges didn't write their files. Verify (`test -f output/domain-scorecard-deployer.md`) was never run by orchestra.
3. **Finals produced no scorecard** — same story. `test -f output/scorecard.md` would have caught it.

## Current Behavior

| Area | Status |
|---|---|
| Verify commands | Prompt instructions only — agents are told to self-verify, orchestra never checks |
| Team completion | Binary — process exits 0 = "done", non-zero = "failed". No deliverable validation |
| Retry | None — failed team kills the entire tier, no auto-retry |
| Self-correction | Agent-driven only — team lead can message teammates to fix, but no orchestrator-level loop |
| Monitoring | Real-time progress lines + `status` command, but no anomaly detection |

## Proposed Layers

### Layer 1: Know what happened (observability)
- **Post-completion verify check** — orchestra runs the `verify` commands itself after each team finishes. Mark `done` vs `done-unverified` vs `failed-verify`.
- **Duration anomaly detection** — flag teams that finish <10% of peer duration in the same tier.
- **Deliverable existence check** — verify all listed `deliverables` actually exist on disk after completion.
- **Richer `status` output** — show verify pass/fail, deliverable count, anomaly flags.

### Layer 2: Alert on it (escalation)
- Coordinator gets notified of verify failures and anomalies automatically.
- Coordinator can message the human with a structured alert: "build-jobs finished in 10s, 0/5 deliverables present."
- **Tier-level health gate** — don't proceed to next tier if >N% of verify checks fail.

### Layer 3: Fix it (self-correction)
- **Auto-retry on verify failure** (configurable: `max_retries: 2`).
- Re-spawn just the failed team with context: "Your previous run completed but verify failed: `test -f output/domain-scorecard-deployer.md` returned non-zero. The file was not created."
- Escalation strategy on retry (e.g., bump model from sonnet to opus on second attempt).

### Layer 4: Learn from it (post-mortem)
- Structured failure log per team: what verify commands failed, what deliverables are missing, duration vs peers.
- End-of-run report: "3/9 teams had verify failures, 1 team anomalously fast."

## Priority

Layer 1 is the lowest-hanging fruit. Running verify commands post-completion and enriching status output would have caught all 3 issues in this hackathon.
