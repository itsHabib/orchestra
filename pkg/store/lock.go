package store

// LockMode selects whether a run lock is exclusive or shared.
type LockMode int

const (
	// LockExclusive prevents any other exclusive or shared run lock from being acquired.
	LockExclusive LockMode = iota

	// LockShared allows other shared holders but blocks exclusive holders.
	LockShared
)
