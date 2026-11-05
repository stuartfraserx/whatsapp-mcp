package wameow

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"

	"github.com/fmflurry/whatsapp-mcp/internal/redact"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

// syncPatchNames lists every app state collection, address book first.
// whatsmeow fetches them only when the phone shares sync keys or the server
// announces a change, so a fetch that failed once is never retried on its own.
var syncPatchNames = []appstate.WAPatchName{
	appstate.WAPatchCriticalUnblockLow,
	appstate.WAPatchCriticalBlock,
	appstate.WAPatchRegularHigh,
	appstate.WAPatchRegular,
	appstate.WAPatchRegularLow,
}

// appStateBackend is the part of whatsmeow the sync scheduler drives.
type appStateBackend interface {
	LoggedIn() bool
	PatchVersion(ctx context.Context, name appstate.WAPatchName) (uint64, error)
	FetchPatch(ctx context.Context, name appstate.WAPatchName, fullSync bool) (mutations int, err error)
	ContactCounts(ctx context.Context) (named, total int, err error)
}

// syncTiming paces the app state requests: one collection at a time, a few
// seconds apart, and runs at least minRunGap apart.
type syncTiming struct {
	startDelay   time.Duration
	patchGap     time.Duration
	retryBase    time.Duration
	maxAttempts  int
	minRunGap    time.Duration
	fetchTimeout time.Duration
}

var defaultSyncTiming = syncTiming{
	startDelay:   10 * time.Second,
	patchGap:     3 * time.Second,
	retryBase:    15 * time.Second,
	maxAttempts:  3,
	minRunGap:    2 * time.Minute,
	fetchTimeout: 3 * time.Minute,
}

type fetchErrorKind int

const (
	fetchTransient fetchErrorKind = iota
	fetchKeysMissing
	fetchDisconnected
)

func classifyFetchError(err error) fetchErrorKind {
	switch {
	case errors.Is(err, appstate.ErrKeyNotFound):
		return fetchKeysMissing
	case errors.Is(err, whatsmeow.ErrNotConnected), errors.Is(err, whatsmeow.ErrNotLoggedIn),
		errors.Is(err, whatsmeow.ErrIQDisconnected), errors.Is(err, context.Canceled):
		return fetchDisconnected
	default:
		return fetchTransient
	}
}

type appStateSyncer struct {
	backend    appStateBackend
	log        *slog.Logger
	timing     syncTiming
	now        func() time.Time
	sleep      func(ctx context.Context, d time.Duration) error
	onFinished func()

	mu           sync.Mutex
	running      bool
	lastStart    time.Time
	prevStart    time.Time
	lastFinish   time.Time
	autoFullDone bool
	state        wa.ContactSyncState
	results      map[appstate.WAPatchName]wa.PatchSync
}

func newAppStateSyncer(backend appStateBackend, log *slog.Logger, onFinished func()) *appStateSyncer {
	return &appStateSyncer{
		backend:    backend,
		log:        log,
		timing:     defaultSyncTiming,
		now:        time.Now,
		sleep:      sleepCtx,
		onFinished: onFinished,
		state:      wa.ContactSyncNotRun,
		results:    map[appstate.WAPatchName]wa.PatchSync{},
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// tryStart reserves a run. ok is false, with the reason, when a run is in
// progress or the previous one started less than minRunGap ago.
func (s *appStateSyncer) tryStart() (wa.ResyncOutcome, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	switch {
	case s.running:
		return wa.ResyncAlreadyRunning, false
	case !s.lastStart.IsZero() && now.Sub(s.lastStart) < s.timing.minRunGap:
		return wa.ResyncTooSoon, false
	}
	s.running = true
	s.prevStart, s.lastStart = s.lastStart, now
	s.state = wa.ContactSyncRunning
	return wa.ResyncStarted, true
}

// startAuto runs the post-connect sync in the background. The address book is
// fully re-downloaded once per process when the store holds no saved name.
func (s *appStateSyncer) startAuto(ctx context.Context) bool {
	if _, ok := s.tryStart(); !ok {
		return false
	}
	go s.run(ctx, false, s.timing.startDelay)
	return true
}

// startManual runs a sync that always re-downloads the address book.
func (s *appStateSyncer) startManual(ctx context.Context) wa.ResyncOutcome {
	outcome, ok := s.tryStart()
	if ok {
		go s.run(ctx, true, 0)
	}
	return outcome
}

// run must follow a successful tryStart.
func (s *appStateSyncer) run(ctx context.Context, forceAddressBook bool, delay time.Duration) {
	out := s.runPatches(ctx, forceAddressBook, delay)
	named, total, err := s.backend.ContactCounts(ctx)
	if err != nil {
		s.log.Warn("count contacts failed", "error", redact.Scrub(err.Error()))
	}
	s.mu.Lock()
	s.running = false
	s.lastFinish = s.now()
	s.state = out.state
	if !out.fetched {
		// Nothing reached WhatsApp, so the next connect may start at once.
		s.lastStart = s.prevStart
	}
	s.mu.Unlock()
	s.log.Info("address book sync finished", "state", string(out.state),
		"contacts_total", total, "contacts_with_name", named)
	if out.synced && s.onFinished != nil {
		s.onFinished()
	}
}

type runOutcome struct {
	state   wa.ContactSyncState
	fetched bool
	synced  bool
}

func (s *appStateSyncer) runPatches(ctx context.Context, forceAddressBook bool, delay time.Duration) runOutcome {
	out := runOutcome{state: wa.ContactSyncNotConnected}
	if delay > 0 && s.sleep(ctx, delay) != nil {
		return out
	}
	if !s.backend.LoggedIn() {
		return out
	}
	fullAddressBook := forceAddressBook
	if !forceAddressBook {
		fullAddressBook = s.needsAutoFullAddressBook(ctx)
	}

	out.state = wa.ContactSyncDone
	for i, name := range syncPatchNames {
		if i > 0 && s.sleep(ctx, s.timing.patchGap) != nil {
			s.markSkipped(syncPatchNames[i:])
			out.state = wa.ContactSyncNotConnected
			return out
		}
		res, kind := s.syncPatch(ctx, name, fullAddressBook && name == appstate.WAPatchCriticalUnblockLow)
		s.record(name, res)
		out.fetched = out.fetched || res.LastResult != wa.PatchSkipped
		out.synced = out.synced || res.LastResult == wa.PatchSynced
		switch {
		case res.LastResult == wa.PatchSynced:
		case kind == fetchDisconnected:
			s.markSkipped(syncPatchNames[i+1:])
			out.state = wa.ContactSyncNotConnected
			return out
		case res.LastResult == wa.PatchKeysMissing:
			out.state = wa.ContactSyncKeysMissing
		case out.state == wa.ContactSyncDone:
			out.state = wa.ContactSyncFailed
		}
	}
	return out
}

// needsAutoFullAddressBook reports whether the address book should be fully
// re-downloaded: contacts are stored but none has a saved name, and no
// automatic full download was tried yet in this process.
func (s *appStateSyncer) needsAutoFullAddressBook(ctx context.Context) bool {
	named, total, err := s.backend.ContactCounts(ctx)
	if err != nil || named > 0 || total == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.autoFullDone {
		return false
	}
	s.autoFullDone = true
	return true
}

func (s *appStateSyncer) syncPatch(ctx context.Context, name appstate.WAPatchName, forceFull bool) (wa.PatchSync, fetchErrorKind) {
	res := wa.PatchSync{Name: string(name)}
	version, err := s.backend.PatchVersion(ctx, name)
	if err != nil {
		res.LastResult, res.Error = wa.PatchFailed, redact.Scrub(err.Error())
		return res, fetchTransient
	}
	res.FullSync = forceFull || version == 0
	var kind fetchErrorKind
	for attempt := 0; attempt < s.timing.maxAttempts; attempt++ {
		if attempt > 0 && s.sleep(ctx, s.timing.retryBase*time.Duration(pow3(attempt-1))) != nil {
			return res, fetchDisconnected
		}
		if !s.backend.LoggedIn() {
			res.LastResult, res.Error = wa.PatchSkipped, "not connected"
			return res, fetchDisconnected
		}
		fetchCtx, cancel := context.WithTimeout(ctx, s.timing.fetchTimeout)
		res.Mutations, err = s.backend.FetchPatch(fetchCtx, name, res.FullSync)
		cancel()
		if err == nil {
			res.LastResult, res.Error = wa.PatchSynced, ""
			res.Version, _ = s.backend.PatchVersion(ctx, name)
			s.log.Info("app state patch synced", "patch", string(name), "full_sync", res.FullSync,
				"mutations", res.Mutations, "version", res.Version)
			return res, fetchTransient
		}
		kind = classifyFetchError(err)
		res.LastResult, res.Error = wa.PatchFailed, redact.Scrub(err.Error())
		if kind == fetchKeysMissing {
			res.LastResult = wa.PatchKeysMissing
		}
		s.log.Warn("app state patch sync failed", "patch", string(name), "full_sync", res.FullSync,
			"attempt", attempt+1, "error", res.Error)
		if kind != fetchTransient {
			break
		}
	}
	res.Version = version
	return res, kind
}

func pow3(n int) int {
	p := 1
	for range n {
		p *= 3
	}
	return p
}

func (s *appStateSyncer) record(name appstate.WAPatchName, res wa.PatchSync) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results[name] = res
}

func (s *appStateSyncer) markSkipped(names []appstate.WAPatchName) {
	for _, name := range names {
		s.record(name, wa.PatchSync{Name: string(name), LastResult: wa.PatchSkipped, Error: "not connected"})
	}
}

// status reports the last run with the stored version of every collection.
func (s *appStateSyncer) status(ctx context.Context) wa.ContactSyncStatus {
	s.mu.Lock()
	st := wa.ContactSyncStatus{State: s.state, LastStarted: s.lastStart, LastFinished: s.lastFinish}
	results := make(map[appstate.WAPatchName]wa.PatchSync, len(s.results))
	for k, v := range s.results {
		results[k] = v
	}
	s.mu.Unlock()

	st.ContactsNamed, st.ContactsTotal, _ = s.backend.ContactCounts(ctx)
	for _, name := range syncPatchNames {
		p := results[name]
		p.Name = string(name)
		if v, err := s.backend.PatchVersion(ctx, name); err == nil {
			p.Version = v
		}
		st.Patches = append(st.Patches, p)
	}
	return st
}
