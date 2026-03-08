package executor

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"miniflow/internal/model"

	"github.com/google/uuid"
)

// RunStore defines the storage interface the executor depends on.
// The real SQLite store (built concurrently) will satisfy this interface.
type RunStore interface {
	GetRun(id string) (*model.WorkflowRun, error)
	UpdateRun(r *model.WorkflowRun) error
	GetWorkflow(id string) (*model.Workflow, error)
	CreateActivityRun(ar *model.ActivityRun) error
	GetActivityRunsByRunID(runID string) ([]model.ActivityRun, error)
	AppendEvent(e *model.Event) error
	ListRuns(status string, limit int) ([]model.WorkflowRun, error)
}

// Executor drives workflow runs forward by inspecting state and scheduling
// the next activity or completing/failing the run.
type Executor struct {
	store RunStore
}

// NewExecutor returns an Executor backed by the given store.
func NewExecutor(store RunStore) *Executor {
	return &Executor{store: store}
}

// StartRun transitions a pending run to running, emits a workflow_started
// event, and calls AdvanceRun to kick off the first activity.
func (e *Executor) StartRun(runID string) error {
	run, err := e.store.GetRun(runID)
	if err != nil {
		return fmt.Errorf("start run: get run: %w", err)
	}

	now := time.Now().UTC()
	run.Status = model.StatusRunning
	run.StartedAt = &now

	if err := e.store.UpdateRun(run); err != nil {
		return fmt.Errorf("start run: update run: %w", err)
	}

	if err := e.store.AppendEvent(&model.Event{
		WorkflowRunID: run.ID,
		EventType:     model.EventWorkflowStarted,
		CreatedAt:     now,
	}); err != nil {
		return fmt.Errorf("start run: append event: %w", err)
	}

	return e.AdvanceRun(runID)
}

// CancelRun marks a run as cancelled and emits a workflow_cancelled event.
func (e *Executor) CancelRun(runID string) error {
	run, err := e.store.GetRun(runID)
	if err != nil {
		return fmt.Errorf("cancel run: get run: %w", err)
	}

	now := time.Now().UTC()
	run.Status = model.StatusCancelled
	run.CompletedAt = &now

	if err := e.store.UpdateRun(run); err != nil {
		return fmt.Errorf("cancel run: update run: %w", err)
	}

	return e.store.AppendEvent(&model.Event{
		WorkflowRunID: run.ID,
		EventType:     model.EventWorkflowCancelled,
		CreatedAt:     now,
	})
}

// AdvanceRun inspects the current state of a workflow run and takes the
// appropriate next action: schedule an activity, advance to the next step,
// complete the run, or fail the run.
func (e *Executor) AdvanceRun(runID string) error {
	run, err := e.store.GetRun(runID)
	if err != nil {
		return fmt.Errorf("advance run: get run: %w", err)
	}

	if run.Status != model.StatusRunning {
		return nil // nothing to do for non-running workflows
	}

	wf, err := e.store.GetWorkflow(run.WorkflowID)
	if err != nil {
		return fmt.Errorf("advance run: get workflow: %w", err)
	}

	activities := wf.Definition.Activities
	if len(activities) == 0 {
		// No activities defined — mark run completed immediately.
		return e.completeRun(run, "")
	}

	activityRuns, err := e.store.GetActivityRunsByRunID(runID)
	if err != nil {
		return fmt.Errorf("advance run: get activity runs: %w", err)
	}

	stepIndex := run.CurrentStep

	// Guard: if current step is past the last activity, the run is done.
	if stepIndex >= len(activities) {
		lastOutput := ""
		if ar := findActivityRunByStep(activityRuns, stepIndex-1); ar != nil {
			lastOutput = ar.Output
		}
		return e.completeRun(run, lastOutput)
	}

	currentSpec := activities[stepIndex]
	currentAR := findActivityRunByStep(activityRuns, stepIndex)

	if currentAR == nil {
		// No activity run for this step yet — schedule one.
		input := resolveInputExpr(currentSpec.InputExpr, run, activityRuns)
		return e.scheduleActivity(run, currentSpec, stepIndex, input)
	}

	switch currentAR.Status {
	case model.StatusCompleted:
		// Move to the next step.
		nextStep := stepIndex + 1
		run.CurrentStep = nextStep

		if nextStep >= len(activities) {
			// That was the last activity — complete the run.
			run.Output = currentAR.Output
			return e.completeRun(run, currentAR.Output)
		}

		if err := e.store.UpdateRun(run); err != nil {
			return fmt.Errorf("advance run: update run step: %w", err)
		}

		// Refresh activity runs since we may need them for input resolution.
		activityRuns, err = e.store.GetActivityRunsByRunID(runID)
		if err != nil {
			return fmt.Errorf("advance run: refresh activity runs: %w", err)
		}

		nextSpec := activities[nextStep]
		input := resolveInputExpr(nextSpec.InputExpr, run, activityRuns)
		return e.scheduleActivity(run, nextSpec, nextStep, input)

	case model.StatusFailed:
		if currentAR.Attempts >= currentAR.MaxRetries+1 {
			// Retries exhausted — fail the workflow run.
			return e.failRun(run, fmt.Sprintf("activity %q failed after %d attempts", currentAR.ActivityName, currentAR.Attempts))
		}
		// Otherwise, still has retries — do nothing (worker will retry).
		return nil

	case model.StatusPending, model.StatusRunning:
		// Activity is in progress — nothing to do.
		return nil

	default:
		return nil
	}
}

// scheduleActivity creates a new ActivityRun and emits an activity_scheduled event.
func (e *Executor) scheduleActivity(run *model.WorkflowRun, spec model.ActivitySpec, stepIndex int, input string) error {
	now := time.Now().UTC()
	ar := &model.ActivityRun{
		ID:             uuid.New().String(),
		WorkflowRunID:  run.ID,
		ActivityName:   spec.Name,
		ActivityType:   spec.Type,
		StepIndex:      stepIndex,
		Status:         model.StatusPending,
		Input:          input,
		Attempts:       0,
		MaxRetries:     spec.MaxRetries,
		TimeoutSeconds: spec.TimeoutSeconds,
		CreatedAt:      now,
	}

	if err := e.store.CreateActivityRun(ar); err != nil {
		return fmt.Errorf("schedule activity: create activity run: %w", err)
	}

	return e.store.AppendEvent(&model.Event{
		WorkflowRunID: run.ID,
		ActivityRunID: ar.ID,
		EventType:     model.EventActivityScheduled,
		Payload:       input,
		CreatedAt:     now,
	})
}

// completeRun marks a workflow run as completed and emits a workflow_completed event.
func (e *Executor) completeRun(run *model.WorkflowRun, output string) error {
	now := time.Now().UTC()
	run.Status = model.StatusCompleted
	run.Output = output
	run.CompletedAt = &now

	if err := e.store.UpdateRun(run); err != nil {
		return fmt.Errorf("complete run: update run: %w", err)
	}

	return e.store.AppendEvent(&model.Event{
		WorkflowRunID: run.ID,
		EventType:     model.EventWorkflowCompleted,
		Payload:       output,
		CreatedAt:     now,
	})
}

// failRun marks a workflow run as failed and emits a workflow_failed event.
func (e *Executor) failRun(run *model.WorkflowRun, reason string) error {
	now := time.Now().UTC()
	run.Status = model.StatusFailed
	run.CompletedAt = &now

	if err := e.store.UpdateRun(run); err != nil {
		return fmt.Errorf("fail run: update run: %w", err)
	}

	return e.store.AppendEvent(&model.Event{
		WorkflowRunID: run.ID,
		EventType:     model.EventWorkflowFailed,
		Payload:       reason,
		CreatedAt:     now,
	})
}

// findActivityRunByStep returns the activity run at the given step index, or nil.
func findActivityRunByStep(runs []model.ActivityRun, stepIndex int) *model.ActivityRun {
	for i := range runs {
		if runs[i].StepIndex == stepIndex {
			return &runs[i]
		}
	}
	return nil
}

// stepsOutputRegex matches expressions like $.steps[0].output
var stepsOutputRegex = regexp.MustCompile(`^\$\.steps\[(\d+)\]\.output$`)

// resolveInputExpr evaluates a simple expression against the run context.
//
//	"$.input"              -> the workflow run's input
//	"$.steps[N].output"    -> the Nth activity run's output (by step_index)
//	""  / anything else    -> the workflow run's input (default)
func resolveInputExpr(expr string, run *model.WorkflowRun, activityRuns []model.ActivityRun) string {
	switch {
	case expr == "$.input":
		return run.Input

	case stepsOutputRegex.MatchString(expr):
		matches := stepsOutputRegex.FindStringSubmatch(expr)
		idx, _ := strconv.Atoi(matches[1])
		if ar := findActivityRunByStep(activityRuns, idx); ar != nil {
			return ar.Output
		}
		return ""

	default:
		return run.Input
	}
}
