package activity

import (
	"context"
	"log"
	"sync"
	"time"

	"miniflow/internal/model"
)

// ActivityStore is the interface the worker pool needs from the persistence layer.
type ActivityStore interface {
	ClaimNextActivity() (*model.ActivityRun, error)
	UpdateActivityRun(ar *model.ActivityRun) error
	AppendEvent(e *model.Event) error
}

// WorkerPool polls the store for pending activities and executes them.
type WorkerPool struct {
	store    ActivityStore
	registry *Registry
	count    int
	interval time.Duration
	quit     chan struct{}
	wg       sync.WaitGroup
}

// NewWorkerPool creates a new WorkerPool.
func NewWorkerPool(store ActivityStore, registry *Registry, count int, interval time.Duration) *WorkerPool {
	return &WorkerPool{
		store:    store,
		registry: registry,
		count:    count,
		interval: interval,
		quit:     make(chan struct{}),
	}
}

// Start launches count worker goroutines, each polling for activities.
func (wp *WorkerPool) Start() {
	for i := 0; i < wp.count; i++ {
		wp.wg.Add(1)
		go wp.workerLoop()
	}
}

// Stop signals all workers to quit and waits for them to finish.
func (wp *WorkerPool) Stop() {
	close(wp.quit)
	wp.wg.Wait()
}

func (wp *WorkerPool) workerLoop() {
	defer wp.wg.Done()
	for {
		select {
		case <-wp.quit:
			return
		default:
		}

		ar, err := wp.store.ClaimNextActivity()
		if err != nil {
			log.Printf("worker: error claiming activity: %v", err)
			wp.sleep()
			continue
		}
		if ar == nil {
			wp.sleep()
			continue
		}

		wp.executeActivity(ar)
	}
}

func (wp *WorkerPool) sleep() {
	select {
	case <-wp.quit:
	case <-time.After(wp.interval):
	}
}

func (wp *WorkerPool) executeActivity(ar *model.ActivityRun) {
	// Set up context with timeout if configured.
	ctx, cancel := context.Background(), func() {}
	if ar.TimeoutSeconds > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(ar.TimeoutSeconds)*time.Second)
	}
	defer cancel()

	// Mark as running.
	now := time.Now()
	ar.Status = model.StatusRunning
	ar.StartedAt = &now
	_ = wp.store.UpdateActivityRun(ar)
	_ = wp.store.AppendEvent(&model.Event{
		WorkflowRunID: ar.WorkflowRunID,
		ActivityRunID: ar.ID,
		EventType:     model.EventActivityStarted,
		CreatedAt:     time.Now(),
	})

	// Start heartbeat goroutine.
	heartDone := make(chan struct{})
	go wp.heartbeat(ar, heartDone)

	// Execute the activity.
	result, execErr := wp.registry.Execute(ctx, ar.ActivityType, ar.Input)

	// Stop heartbeat.
	close(heartDone)

	ar.Attempts++
	completedAt := time.Now()

	if execErr == nil {
		// Success.
		ar.Status = model.StatusCompleted
		ar.Output = result
		ar.CompletedAt = &completedAt
		_ = wp.store.UpdateActivityRun(ar)
		_ = wp.store.AppendEvent(&model.Event{
			WorkflowRunID: ar.WorkflowRunID,
			ActivityRunID: ar.ID,
			EventType:     model.EventActivityCompleted,
			CreatedAt:     time.Now(),
		})
		return
	}

	// Determine if this was a timeout.
	isTimeout := ctx.Err() != nil

	if ar.Attempts >= ar.MaxRetries {
		// Exhausted retries.
		ar.Status = model.StatusFailed
		ar.Output = execErr.Error()
		ar.CompletedAt = &completedAt
		_ = wp.store.UpdateActivityRun(ar)
		eventType := model.EventActivityFailed
		if isTimeout {
			eventType = model.EventActivityTimedOut
		}
		_ = wp.store.AppendEvent(&model.Event{
			WorkflowRunID: ar.WorkflowRunID,
			ActivityRunID: ar.ID,
			EventType:     eventType,
			CreatedAt:     time.Now(),
		})
		return
	}

	// Retry: reset to pending.
	ar.Status = model.StatusPending
	ar.StartedAt = nil
	_ = wp.store.UpdateActivityRun(ar)
	eventType := model.EventActivityRetried
	if isTimeout {
		eventType = model.EventActivityTimedOut
	}
	_ = wp.store.AppendEvent(&model.Event{
		WorkflowRunID: ar.WorkflowRunID,
		ActivityRunID: ar.ID,
		EventType:     eventType,
		CreatedAt:     time.Now(),
	})
}

func (wp *WorkerPool) heartbeat(ar *model.ActivityRun, done chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-wp.quit:
			return
		case <-ticker.C:
			now := time.Now()
			ar.LastHeartbeat = &now
			_ = wp.store.UpdateActivityRun(ar)
			_ = wp.store.AppendEvent(&model.Event{
				WorkflowRunID: ar.WorkflowRunID,
				ActivityRunID: ar.ID,
				EventType:     model.EventActivityHeartbeat,
				CreatedAt:     now,
			})
		}
	}
}
