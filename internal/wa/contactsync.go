package wa

import "time"

// ResyncOutcome tells whether a requested address-book sync was started.
type ResyncOutcome string

// Resync outcomes.
const (
	ResyncStarted        ResyncOutcome = "started"
	ResyncAlreadyRunning ResyncOutcome = "already_running"
	ResyncTooSoon        ResyncOutcome = "too_soon"
)

// ContactSyncState summarises the last address-book (app state) sync run.
type ContactSyncState string

// Contact sync states.
const (
	ContactSyncNotRun       ContactSyncState = "not_run"
	ContactSyncRunning      ContactSyncState = "running"
	ContactSyncDone         ContactSyncState = "done"
	ContactSyncKeysMissing  ContactSyncState = "keys_missing"
	ContactSyncFailed       ContactSyncState = "failed"
	ContactSyncNotConnected ContactSyncState = "not_connected"
)

// Results of one app state patch fetch.
const (
	PatchSynced      = "synced"
	PatchKeysMissing = "keys_missing"
	PatchFailed      = "failed"
	PatchSkipped     = "skipped"
)

// PatchSync is the state of one app state collection. Version 0 means the
// collection was never synchronised. LastResult is empty until this process
// tried to fetch it.
type PatchSync struct {
	Name       string
	Version    uint64
	LastResult string
	FullSync   bool
	Mutations  int
	Error      string
}

// ContactSyncStatus reports the address-book sync: counts only, never names
// or numbers.
type ContactSyncStatus struct {
	State         ContactSyncState
	ContactsTotal int
	ContactsNamed int
	LastStarted   time.Time
	LastFinished  time.Time
	Patches       []PatchSync
}
