package executor

import (
	"log"
	"time"
)

// Scheduler periodically polls the store for pending and running workflow
// runs and drives them forward via the Executor.
type Scheduler struct {
	executor *Executor
	store    RunStore
	interval time.Duration
	quit     chan struct{}
}

// NewScheduler creates a Scheduler that ticks at the given interval.
func NewScheduler(executor *Executor, store RunStore, interval time.Duration) *Scheduler {
	return &Scheduler{
		executor: executor,
		store:    store,
		interval: interval,
		quit:     make(chan struct{}),
	}
}

// Start begins polling in a background goroutine.
func (s *Scheduler) Start() {
	go s.loop()
}

// Stop signals the polling goroutine to exit and returns immediately.
func (s *Scheduler) Stop() {
	close(s.quit)
}

func (s *Scheduler) loop() {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.quit:
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

func (s *Scheduler) tick() {
	// Start any pending runs.
	pending, err := s.store.ListRuns("pending", 100)
	if err != nil {
		log.Printf("scheduler: list pending runs: %v", err)
	} else {
		for _, run := range pending {
			if err := s.executor.StartRun(run.ID); err != nil {
				log.Printf("scheduler: start run %s: %v", run.ID, err)
			}
		}
	}

	// Advance any running runs.
	running, err := s.store.ListRuns("running", 100)
	if err != nil {
		log.Printf("scheduler: list running runs: %v", err)
	} else {
		for _, run := range running {
			if err := s.executor.AdvanceRun(run.ID); err != nil {
				log.Printf("scheduler: advance run %s: %v", run.ID, err)
			}
		}
	}
}
