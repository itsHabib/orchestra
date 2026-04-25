package spawner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/itsHabib/orchestra/internal/store/memstore"
)

func TestStartSession_ConcurrencyCapHonored(t *testing.T) {
	const capacity = 3
	const total = 8

	sessions := newBlockingSessionAPI()
	sp := newManagedAgentsSpawnerWithSessions(
		memstore.New(),
		newFakeAgentAPI(),
		newFakeEnvAPI(),
		sessions,
		&fakeSessionEventsAPI{},
		WithManagedAgentsConcurrency(capacity),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(total)
	for i := 0; i < total; i++ {
		go func() {
			defer wg.Done()
			_, err := sp.StartSession(ctx, StartSessionRequest{
				Agent:    AgentHandle{ID: fmt.Sprintf("agent_%d", i), Version: 1},
				Env:      EnvHandle{ID: "env_1"},
				TeamName: fmt.Sprintf("team_%d", i),
			})
			if err != nil {
				t.Errorf("StartSession[%d]: %v", i, err)
			}
		}()
	}

	if err := sessions.WaitForInflight(ctx, capacity); err != nil {
		t.Fatalf("waiting for capacity inflight: %v", err)
	}
	// Hold for a moment so a stray goroutine has a chance to overshoot.
	time.Sleep(50 * time.Millisecond)
	if peak := sessions.Peak(); peak > capacity {
		t.Fatalf("peak inflight=%d, want <= %d", peak, capacity)
	}

	sessions.ReleaseAll()
	wg.Wait()

	if peak := sessions.Peak(); peak > capacity {
		t.Fatalf("post-run peak inflight=%d, want <= %d", peak, capacity)
	}
	if calls := sessions.Calls(); calls != total {
		t.Fatalf("session.New calls=%d, want %d", calls, total)
	}
}

func TestStartSession_ReleasesSlotOnError(t *testing.T) {
	sessions := newBlockingSessionAPI()
	sessions.SetError(errors.New("boom"))
	sp := newManagedAgentsSpawnerWithSessions(
		memstore.New(),
		newFakeAgentAPI(),
		newFakeEnvAPI(),
		sessions,
		&fakeSessionEventsAPI{},
		WithManagedAgentsConcurrency(1),
		WithManagedAgentsConfig(ManagedAgentsConfig{
			APIMaxAttempts:    1,
			APIRetryBaseDelay: time.Nanosecond,
			APIRetryMaxDelay:  time.Nanosecond,
		}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// First call drains the only slot and fails.
	if _, err := sp.StartSession(ctx, StartSessionRequest{
		Agent:    AgentHandle{ID: "agent_1", Version: 1},
		Env:      EnvHandle{ID: "env_1"},
		TeamName: "team_1",
	}); err == nil {
		t.Fatal("expected first StartSession to fail")
	}

	// Second call must proceed — if the slot leaked, this would block until ctx
	// times out and fail with context.DeadlineExceeded instead of "boom".
	if _, err := sp.StartSession(ctx, StartSessionRequest{
		Agent:    AgentHandle{ID: "agent_2", Version: 1},
		Env:      EnvHandle{ID: "env_1"},
		TeamName: "team_2",
	}); err == nil {
		t.Fatal("expected second StartSession to fail with the same boom")
	}

	if calls := sessions.Calls(); calls != 2 {
		t.Fatalf("session.New calls=%d, want 2 (slot leak would block call 2)", calls)
	}
}

// blockingSessionAPI implements managedSessionAPI with explicit release. New
// blocks until ReleaseAll (or context cancel) so a test can observe how many
// callers are simultaneously inside New.
type blockingSessionAPI struct {
	mu       sync.Mutex
	gate     chan struct{}
	inflight int32
	peak     int32
	calls    int32
	err      error
}

func newBlockingSessionAPI() *blockingSessionAPI {
	return &blockingSessionAPI{gate: make(chan struct{})}
}

func (f *blockingSessionAPI) SetError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
	close(f.gate) // failing path returns immediately; no waiter to wake
	f.gate = nil
}

//nolint:gocritic // fake API mirrors the SDK method shape used by production code.
func (f *blockingSessionAPI) New(ctx context.Context, _ anthropic.BetaSessionNewParams, _ ...option.RequestOption) (*anthropic.BetaManagedAgentsSession, error) {
	atomic.AddInt32(&f.calls, 1)
	cur := atomic.AddInt32(&f.inflight, 1)
	defer atomic.AddInt32(&f.inflight, -1)
	for {
		old := atomic.LoadInt32(&f.peak)
		if cur <= old || atomic.CompareAndSwapInt32(&f.peak, old, cur) {
			break
		}
	}

	f.mu.Lock()
	gate := f.gate
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &anthropic.BetaManagedAgentsSession{ID: fmt.Sprintf("sess_%d", atomic.LoadInt32(&f.calls))}, nil
}

func (f *blockingSessionAPI) Get(context.Context, string, anthropic.BetaSessionGetParams, ...option.RequestOption) (*anthropic.BetaManagedAgentsSession, error) {
	return &anthropic.BetaManagedAgentsSession{}, nil
}

func (f *blockingSessionAPI) Inflight() int { return int(atomic.LoadInt32(&f.inflight)) }
func (f *blockingSessionAPI) Peak() int     { return int(atomic.LoadInt32(&f.peak)) }
func (f *blockingSessionAPI) Calls() int    { return int(atomic.LoadInt32(&f.calls)) }

func (f *blockingSessionAPI) ReleaseAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gate != nil {
		close(f.gate)
		f.gate = nil
	}
}

// WaitForInflight returns when at least n goroutines are simultaneously inside
// New. Returns the context error if it times out.
func (f *blockingSessionAPI) WaitForInflight(ctx context.Context, n int) error {
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		if int(atomic.LoadInt32(&f.inflight)) >= n {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
