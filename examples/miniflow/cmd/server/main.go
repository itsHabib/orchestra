package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"miniflow/internal/activity"
	"miniflow/internal/config"
	"miniflow/internal/executor"
	"miniflow/internal/handler"
	"miniflow/internal/store"
)

func main() {
	cfg := config.Load()

	s, err := store.NewStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("failed to open store: %v", err)
	}
	defer s.Close()

	// Activity registry with built-in handlers.
	registry := activity.NewRegistry()
	activity.RegisterBuiltins(registry)

	// Activity worker pool.
	workers := activity.NewWorkerPool(s, registry, cfg.WorkerCount, 500*time.Millisecond)
	workers.Start()

	// Workflow executor and scheduler.
	ex := executor.NewExecutor(s)
	sched := executor.NewScheduler(ex, s, 500*time.Millisecond)
	sched.Start()

	// HTTP server.
	router := handler.New(s, ex)
	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: router,
	}

	// Graceful shutdown.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("miniflow server listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-quit
	log.Println("shutting down...")

	sched.Stop()
	workers.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("server shutdown error: %v", err)
	}
	log.Println("miniflow stopped")
}
