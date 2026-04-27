package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LogNotifier appends one NDJSON record per Notification to a path. The
// engine's runtime is the sole writer (see DESIGN-ship-feature-workflow §11.3),
// so a process-local mutex is enough — no gofrs/flock. The file is opened
// with O_APPEND on each call so concurrent writers within the process queue
// behind the mutex without holding a long-lived fd, which would defeat
// `tail -f` consumers.
type LogNotifier struct {
	path string
	mu   sync.Mutex
}

// NewLog returns a LogNotifier that writes to path.
func NewLog(path string) *LogNotifier {
	return &LogNotifier{path: path}
}

// Path returns the destination path. Useful for diagnostics.
func (l *LogNotifier) Path() string { return l.path }

type logRecord struct {
	Timestamp time.Time `json:"ts"`
	RunID     string    `json:"run_id,omitempty"`
	Team      string    `json:"team"`
	Status    string    `json:"status"`
	Summary   string    `json:"summary"`
	PRURL     string    `json:"pr_url,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// Notify appends one NDJSON record to the configured path.
func (l *LogNotifier) Notify(_ context.Context, n *Notification) error {
	if n == nil {
		return nil
	}
	rec := logRecord{
		Timestamp: notificationTimestamp(n.Timestamp),
		RunID:     n.RunID,
		Team:      n.Team,
		Status:    n.Status,
		Summary:   n.Summary,
		PRURL:     n.PRURL,
		Reason:    n.Reason,
	}
	line, err := json.Marshal(&rec)
	if err != nil {
		return fmt.Errorf("notify: marshal: %w", err)
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return fmt.Errorf("notify: prepare log dir: %w", err)
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("notify: open %s: %w", l.path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("notify: write %s: %w", l.path, err)
	}
	return nil
}

func notificationTimestamp(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t.UTC()
}
