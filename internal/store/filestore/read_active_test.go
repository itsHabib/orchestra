package filestore_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/itsHabib/orchestra/internal/store"
	"github.com/itsHabib/orchestra/internal/store/filestore"
)

func TestReadActiveRunState_ReturnsNotFoundWhenAbsent(t *testing.T) {
	ws := filepath.Join(t.TempDir(), ".orchestra")
	_, err := filestore.ReadActiveRunState(context.Background(), ws)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err=%v, want store.ErrNotFound", err)
	}
}

func TestReadActiveRunState_ReturnsAtomicSnapshotWhileLockHeldInProcess(t *testing.T) {
	ctx := context.Background()
	ws := filepath.Join(t.TempDir(), ".orchestra")
	holderStore := filestore.New(ws)
	if err := holderStore.SaveRunState(ctx, &store.RunState{
		Project: "p",
		Backend: "managed_agents",
		Teams: map[string]store.TeamState{
			"alpha": {Status: "running", SessionID: "sess_1"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	release, err := holderStore.AcquireRunLock(ctx, store.LockExclusive)
	if err != nil {
		t.Fatalf("AcquireRunLock: %v", err)
	}
	defer release()

	done := make(chan error, 1)
	go func() {
		_, err := filestore.ReadActiveRunState(ctx, ws)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ReadActiveRunState while lock held: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadActiveRunState blocked while lock was held — the lock-free contract is broken")
	}
}

// TestReadActiveRunState_ReadsWhileSeparateProcessHoldsLock spawns a helper
// binary that acquires the run lock and holds it for several seconds, then
// reads state.json from this process. Verifies the lock-free read works
// across processes (which is what `orchestra msg` relies on while
// `orchestra run` is active and holding the exclusive lock).
//
// Skipped in -short mode because building the helper binary takes a few
// seconds; the in-process variant above covers the cheap fast path.
func TestReadActiveRunState_ReadsWhileSeparateProcessHoldsLock(t *testing.T) {
	if testing.Short() {
		t.Skip("requires building a helper binary")
	}

	ws := filepath.Join(t.TempDir(), ".orchestra")
	helperBin := buildLockHolderHelper(t)

	want := &store.RunState{
		Project: "cross-proc",
		Backend: "managed_agents",
		Teams:   map[string]store.TeamState{"alpha": {Status: "running", SessionID: "sess_x"}},
	}
	holderStore := filestore.New(ws)
	if err := holderStore.SaveRunState(context.Background(), want); err != nil {
		t.Fatal(err)
	}

	signalPath := filepath.Join(t.TempDir(), "lock-acquired")
	const holdSeconds = 3
	helperCtx, cancelHelper := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelHelper()
	cmd := exec.CommandContext(helperCtx, helperBin, ws, signalPath, strconv.Itoa(holdSeconds))
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	if err := waitFor(signalPath, 10*time.Second); err != nil {
		t.Fatalf("helper never signaled lock acquisition: %v", err)
	}

	readStart := time.Now()
	got, err := filestore.ReadActiveRunState(context.Background(), ws)
	if err != nil {
		t.Fatalf("ReadActiveRunState: %v", err)
	}
	elapsed := time.Since(readStart)
	if elapsed > time.Second {
		t.Fatalf("ReadActiveRunState took %s — lock held by separate process appears to block reads", elapsed)
	}
	if got.Project != want.Project || got.Backend != want.Backend {
		t.Fatalf("got=%+v, want %+v", got, want)
	}
	alpha, ok := got.Teams["alpha"]
	if !ok || alpha.SessionID != "sess_x" {
		t.Fatalf("alpha=%+v, want sess_x", alpha)
	}

	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper exited non-zero: %v", err)
	}
}

func buildLockHolderHelper(t *testing.T) string {
	t.Helper()
	binName := "lockholder"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(t.TempDir(), binName)

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(wd, "testdata", "lockholder", "main.go")

	buildCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(buildCtx, goExe(), "build", "-o", binPath, srcPath)
	cmd.Dir = wd
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building lockholder helper: %v\n%s", err, out)
	}
	return binPath
}

func waitFor(path string, deadline time.Duration) error {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", path)
}

func goExe() string {
	name := "go"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	return name
}
