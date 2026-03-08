package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"miniflow/internal/executor"
	"miniflow/internal/model"
	"miniflow/internal/store"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Handler holds dependencies for HTTP endpoints.
type Handler struct {
	store    *store.Store
	executor *executor.Executor
}

// New creates a Handler and returns a chi.Router with all routes mounted.
func New(s *store.Store, ex *executor.Executor) http.Handler {
	h := &Handler{store: s, executor: ex}
	r := chi.NewRouter()

	r.Post("/api/workflows", h.CreateWorkflow)
	r.Get("/api/workflows", h.ListWorkflows)
	r.Get("/api/workflows/{name}", h.GetWorkflow)
	r.Post("/api/workflows/{name}/run", h.StartRun)

	r.Get("/api/runs", h.ListRuns)
	r.Get("/api/runs/{id}", h.GetRun)
	r.Get("/api/runs/{id}/events", h.ListEvents)
	r.Post("/api/runs/{id}/cancel", h.CancelRun)

	r.Get("/api/stats", h.Stats)
	r.Get("/health", h.Health)

	return r
}

func (h *Handler) CreateWorkflow(w http.ResponseWriter, r *http.Request) {
	var req CreateWorkflowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	wf := &model.Workflow{
		ID:         uuid.New().String(),
		Name:       req.Name,
		Definition: req.Definition,
	}
	if err := h.store.CreateWorkflow(wf); err != nil {
		if errors.Is(err, store.ErrDuplicateWorkflow) {
			writeError(w, http.StatusConflict, "workflow name already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, wf)
}

func (h *Handler) ListWorkflows(w http.ResponseWriter, r *http.Request) {
	workflows, err := h.store.ListWorkflows()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, workflows)
}

func (h *Handler) GetWorkflow(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	wf, err := h.store.GetWorkflowByName(name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "workflow not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, wf)
}

func (h *Handler) StartRun(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	wf, err := h.store.GetWorkflowByName(name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "workflow not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var req StartRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		req.Input = "{}"
	}
	if req.Input == "" {
		req.Input = "{}"
	}

	run := &model.WorkflowRun{
		ID:         uuid.New().String(),
		WorkflowID: wf.ID,
		Status:     model.StatusPending,
		Input:      req.Input,
	}
	if err := h.store.CreateRun(run); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

func (h *Handler) ListRuns(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	runs, err := h.store.ListRuns(status, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (h *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	run, err := h.store.GetRun(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "run not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	activities, err := h.store.GetActivityRunsByRunID(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, RunDetailResponse{
		WorkflowRun: *run,
		Activities:  activities,
	})
}

func (h *Handler) ListEvents(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	// Verify run exists.
	if _, err := h.store.GetRun(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "run not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	events, err := h.store.ListEvents(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (h *Handler) CancelRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.executor.CancelRun(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "run not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	run, err := h.store.GetRun(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (h *Handler) Stats(w http.ResponseWriter, r *http.Request) {
	counts, err := h.store.Stats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, StatsResponse{Counts: counts})
}

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg})
}
