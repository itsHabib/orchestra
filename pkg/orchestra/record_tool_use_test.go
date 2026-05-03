package orchestra

import (
	"sync"
	"testing"
)

// TestRecordToolUse pins the dedupe rule that the OnToolUse callback
// uses to skip state.json writes for consecutive identical tool names.
// First write returns true so the cache initializes; the same tool
// fired again returns false so the caller skips the persist. A
// different tool returns true and replaces the cache.
func TestRecordToolUse(t *testing.T) {
	t.Parallel()

	r := &orchestrationRun{}
	if !r.recordToolUse("alpha", "Bash") {
		t.Fatal("first Bash should change the cache")
	}
	if r.recordToolUse("alpha", "Bash") {
		t.Fatal("second consecutive Bash should be deduped")
	}
	if r.recordToolUse("alpha", "Bash") {
		t.Fatal("third consecutive Bash should still be deduped")
	}
	if !r.recordToolUse("alpha", "Edit") {
		t.Fatal("Edit after Bash should change the cache")
	}
	if !r.recordToolUse("alpha", "Bash") {
		t.Fatal("Bash after Edit should change the cache again")
	}
	// Per-agent cache: a different agent's tool name doesn't shadow
	// alpha's — the run can have alpha looping on Edit while beta
	// loops on Bash without crosstalk.
	if !r.recordToolUse("beta", "Edit") {
		t.Fatal("first Edit on beta should change the cache")
	}
	if r.recordToolUse("beta", "Edit") {
		t.Fatal("second consecutive Edit on beta should be deduped")
	}
	if r.recordToolUse("alpha", "Bash") {
		t.Fatal("alpha's last tool is still Bash; beta's Edit shouldn't have changed it")
	}
}

// TestRecordToolUse_ConcurrentAgents stresses the per-agent guard
// under N goroutines hitting the same dedupe map. The mutex must
// serialize the read-modify-write — without it the previous value
// could be lost and dedupe would flap.
func TestRecordToolUse_ConcurrentAgents(t *testing.T) {
	t.Parallel()

	r := &orchestrationRun{}
	const goroutines = 32
	const callsPerGoroutine = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < callsPerGoroutine; j++ {
				_ = r.recordToolUse("alpha", "Bash")
			}
		}()
	}
	wg.Wait()
	// After the storm, alpha's last tool is Bash. Calling it again
	// must dedupe — proves no goroutine left the cache half-written.
	if r.recordToolUse("alpha", "Bash") {
		t.Fatal("post-concurrent: alpha cache should still hold Bash, dedupe expected")
	}
	if !r.recordToolUse("alpha", "Edit") {
		t.Fatal("post-concurrent: alpha→Edit transition should be detected")
	}
}
