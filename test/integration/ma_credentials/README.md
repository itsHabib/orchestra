# ma_credentials — env-injection requirement (gated, currently skipped)

Pins the requirement that `requires_credentials:` declared in `orchestra.yaml`
reaches each agent's Managed Agents sandbox as session env vars.

## Why this directory exists today

Phase A's self-dogfood ([`docs/feedback-phase-a-dogfood.md` §B2][feedback])
found that on the `managed_agents` backend, orchestra resolves credential
names but the secret never reaches the sandbox. PR #33 ("credential
injection") closed this for the **local** backend; the **MA** path hit an
SDK gap (`anthropic-sdk-go` v1.37.0 `BetaSessionNewParams` has no `Env`
field; the Vault credential auth union only supports `mcp_oauth` /
`static_bearer`, not generic env vars).

Closing the gap is tracked in [orchestra#42][issue].

## What this test does

Today: **always skips** with a clear reason pointing at the SDK issue.

Once the SDK exposes env injection and `internal/spawner` plumbs it
through `BetaSessionNewParams`, removing the second `t.Skip` line in
`credentials_test.go` is enough to make the test exercise the full path:

1. Build a single-agent MA config declaring
   `requires_credentials: [ORCHESTRA_MA_TEST_SECRET]`.
2. Drive `orchestra run` with `$ORCHESTRA_MA_TEST_SECRET` set on the host
   so `internal/credentials` resolves it.
3. Have the agent `printenv ORCHESTRA_MA_TEST_SECRET` and surface the
   value via its result summary / artifacts.
4. Assert the per-agent `ResultSummary` in `state.json` carries the host
   value.

The local-backend equivalent already passes:
[`internal/spawner/local_test.go::TestSpawn_EnvOverlayReachesChild`][local-test].

## Running

```bash
ORCHESTRA_MA_INTEGRATION=1 ORCHESTRA_MA_TEST_SECRET=test_value \
  make e2e-ma-credentials
```

Today this prints `--- SKIP` regardless of env (the second skip is
unconditional). Once the SDK gap closes the second skip is removed and
the env vars start mattering.

[feedback]: ../../../docs/feedback-phase-a-dogfood.md
[issue]: https://github.com/itsHabib/orchestra/issues/42
[local-test]: ../../../internal/spawner/local_test.go
