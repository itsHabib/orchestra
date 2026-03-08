package store

import "errors"

// Sentinel errors returned by Store methods.
var (
	// ErrNotFound is returned when a requested entity does not exist.
	ErrNotFound = errors.New("not found")

	// ErrDuplicateWorkflow is returned when creating a workflow with a name
	// that already exists (UNIQUE constraint violation).
	ErrDuplicateWorkflow = errors.New("duplicate workflow name")
)
