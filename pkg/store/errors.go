package store

import "errors"

var (
	// ErrNotFound reports that a requested store document or record does not exist.
	ErrNotFound = errors.New("store: not found")

	// ErrLockTimeout reports that a lock could not be acquired before the context deadline.
	ErrLockTimeout = errors.New("store: lock acquire timed out")

	// ErrLockHeld reports that a run lock is already held by another process.
	ErrLockHeld = errors.New("store: run lock held by another process")

	// ErrArchivedAgent reports that a cached managed-agent resource was archived upstream.
	ErrArchivedAgent = errors.New("store: cached agent is archived on backend")

	// ErrInvalidArgument reports that a caller passed an invalid argument
	// (e.g. a nil record to Put). Distinct from ErrNotFound so callers can
	// tell a missing persisted document apart from bad input.
	ErrInvalidArgument = errors.New("store: invalid argument")
)
