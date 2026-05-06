// Package macredentials pins the requirement that requires_credentials
// reaches the Managed Agents sandbox as session env vars. The test is
// **always skipped today**: the Anthropic SDK's BetaSessionNewParams
// (anthropic-sdk-go v1.37.0) does not yet expose an Env / EnvironmentVariables
// field, so orchestra cannot wire the resolved values through to the
// container. The test is structured so that when the SDK adds the field
// (tracked in github.com/itsHabib/orchestra/issues/42) and orchestra plumbs
// it through, removing the skip line is enough to make the assertion run.
//
// The local-side equivalent already passes:
// internal/spawner/local_test.go::TestSpawn_EnvOverlayReachesChild.
//
// See docs/feedback-phase-a-dogfood.md §B2 for the dogfood finding that
// surfaced the scope.
package macredentials

import (
	"os"
	"testing"
)

const (
	envIntegration = "ORCHESTRA_MA_INTEGRATION"
	envSecret      = "ORCHESTRA_MA_TEST_SECRET"
)

// TestRequiresCredentialsReachMASandbox is the placeholder for the live-MA
// env-injection assertion. The body is intentionally a no-op pending the
// SDK gap closing — see the package doc for the rationale and tracking
// issue.
//
// When SDK + orchestra wiring catches up, the body should:
//
//  1. Build a tiny orchestra config (single agent, MA backend) that declares
//     requires_credentials: [ORCHESTRA_MA_TEST_SECRET].
//  2. Drive `orchestra run` against that config with $ORCHESTRA_MA_TEST_SECRET
//     set in the host env (so internal/credentials resolves it).
//  3. Have the agent's task ask it to call `printenv ORCHESTRA_MA_TEST_SECRET`
//     and put the value in its result_summary / artifacts.
//  4. Assert state.json's per-agent ResultSummary contains the value the host
//     set in step 2.
//
// Mirrors internal/spawner/local_test.go::TestSpawn_EnvOverlayReachesChild
// for the local backend, which is already green.
func TestRequiresCredentialsReachMASandbox(t *testing.T) {
	if os.Getenv(envIntegration) != "1" {
		t.Skipf("set %s=1 to enable", envIntegration)
	}
	t.Skip(
		"managed-agents SDK does not yet expose per-session env-var injection " +
			"(anthropic-sdk-go v1.37.0 BetaSessionNewParams has no Env field). " +
			"Tracking: https://github.com/itsHabib/orchestra/issues/42. " +
			"Remove this skip line once the SDK + spawner wiring lands.",
	)
}
