package wameow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

type fetchCall struct {
	name appstate.WAPatchName
	full bool
}

type fakeAppState struct {
	mu        sync.Mutex
	loggedIn  bool
	versions  map[appstate.WAPatchName]uint64
	errs      map[appstate.WAPatchName][]error
	calls     []fetchCall
	named     int
	total     int
	onFetch   func(name appstate.WAPatchName)
	mutations int
}

func newFakeAppState() *fakeAppState {
	versions := map[appstate.WAPatchName]uint64{}
	for _, n := range syncPatchNames {
		versions[n] = 7
	}
	return &fakeAppState{loggedIn: true, versions: versions, errs: map[appstate.WAPatchName][]error{}, named: 10, total: 20, mutations: 3}
}

func (f *fakeAppState) LoggedIn() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loggedIn
}

func (f *fakeAppState) PatchVersion(_ context.Context, name appstate.WAPatchName) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.versions[name], nil
}

func (f *fakeAppState) FetchPatch(_ context.Context, name appstate.WAPatchName, full bool) (int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fetchCall{name, full})
	var err error
	if q := f.errs[name]; len(q) > 0 {
		err, f.errs[name] = q[0], q[1:]
	}
	if err == nil {
		f.versions[name]++
	}
	hook := f.onFetch
	f.mu.Unlock()
	if hook != nil {
		hook(name)
	}
	if err != nil {
		return 0, err
	}
	return f.mutations, nil
}

func (f *fakeAppState) ContactCounts(context.Context) (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.named, f.total, nil
}

func (f *fakeAppState) fetchCalls() []fetchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

type syncHarness struct {
	*appStateSyncer
	mu       sync.Mutex
	now      time.Time
	sleeps   []time.Duration
	finished chan struct{}
}

func newSyncHarness(backend appStateBackend) *syncHarness {
	h := &syncHarness{now: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC), finished: make(chan struct{}, 10)}
	s := newAppStateSyncer(backend, slog.New(slog.NewTextHandler(io.Discard, nil)), func() { h.finished <- struct{}{} })
	s.now = func() time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.now
	}
	s.sleep = func(ctx context.Context, d time.Duration) error {
		h.mu.Lock()
		h.sleeps = append(h.sleeps, d)
		h.mu.Unlock()
		return ctx.Err()
	}
	h.appStateSyncer = s
	return h
}

func (h *syncHarness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

func (h *syncHarness) sleptFor() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.sleeps)
}

// runOnce reserves and runs one sync synchronously.
func (h *syncHarness) runOnce(t *testing.T, force bool) {
	t.Helper()
	if outcome, ok := h.tryStart(); !ok {
		t.Fatalf("tryStart = %s, want started", outcome)
	}
	h.run(context.Background(), force, 0)
}

func callsFor(names []appstate.WAPatchName, full map[appstate.WAPatchName]bool) []fetchCall {
	out := make([]fetchCall, 0, len(names))
	for _, n := range names {
		out = append(out, fetchCall{n, full[n]})
	}
	return out
}

func TestSyncPatchNamesCoverEveryWhatsmeowCollection(t *testing.T) {
	for _, n := range appstate.AllPatchNames {
		if !slices.Contains(syncPatchNames, n) {
			t.Errorf("syncPatchNames misses %q", n)
		}
	}
	if len(syncPatchNames) != len(appstate.AllPatchNames) {
		t.Errorf("syncPatchNames = %v, want exactly %v", syncPatchNames, appstate.AllPatchNames)
	}
	if syncPatchNames[0] != appstate.WAPatchCriticalUnblockLow {
		t.Errorf("first patch = %q, want the address book (critical_unblock_low)", syncPatchNames[0])
	}
}

func TestSyncFetchesEveryPatchOnceSequentiallyAndIncrementallyWhenSynced(t *testing.T) {
	backend := newFakeAppState()
	h := newSyncHarness(backend)

	h.runOnce(t, false)

	if got, want := backend.fetchCalls(), callsFor(syncPatchNames, nil); !slices.Equal(got, want) {
		t.Fatalf("fetches = %v, want %v", got, want)
	}
	gaps := slices.Repeat([]time.Duration{defaultSyncTiming.patchGap}, len(syncPatchNames)-1)
	if got := h.sleptFor(); !slices.Equal(got, gaps) {
		t.Errorf("sleeps = %v, want a %v gap between patches %v", got, defaultSyncTiming.patchGap, gaps)
	}
	st := h.status(context.Background())
	if st.State != wa.ContactSyncDone {
		t.Errorf("state = %s, want done", st.State)
	}
	for _, p := range st.Patches {
		if p.LastResult != wa.PatchSynced || p.Version != 8 || p.Mutations != 3 || p.FullSync {
			t.Errorf("patch %+v, want synced incrementally to version 8 with 3 mutations", p)
		}
	}
}

func TestSyncRequestsAFullSyncForCollectionsNeverSynced(t *testing.T) {
	backend := newFakeAppState()
	backend.versions = map[appstate.WAPatchName]uint64{appstate.WAPatchCriticalBlock: 4, appstate.WAPatchRegularLow: 104}
	h := newSyncHarness(backend)

	h.runOnce(t, false)

	want := callsFor(syncPatchNames, map[appstate.WAPatchName]bool{
		appstate.WAPatchCriticalUnblockLow: true, appstate.WAPatchRegularHigh: true, appstate.WAPatchRegular: true,
	})
	if got := backend.fetchCalls(); !slices.Equal(got, want) {
		t.Fatalf("fetches = %v, want %v", got, want)
	}
}

func TestAutoSyncRedownloadsTheAddressBookOncePerProcessWhenNoNameIsStored(t *testing.T) {
	backend := newFakeAppState()
	backend.named, backend.total = 0, 1164
	h := newSyncHarness(backend)

	h.runOnce(t, false)
	h.advance(defaultSyncTiming.minRunGap)
	h.runOnce(t, false)

	calls := backend.fetchCalls()
	first, second := calls[:len(syncPatchNames)], calls[len(syncPatchNames):]
	if want := callsFor(syncPatchNames, map[appstate.WAPatchName]bool{appstate.WAPatchCriticalUnblockLow: true}); !slices.Equal(first, want) {
		t.Errorf("first run = %v, want a full address-book sync %v", first, want)
	}
	if want := callsFor(syncPatchNames, nil); !slices.Equal(second, want) {
		t.Errorf("second run = %v, want incremental only %v", second, want)
	}
}

func TestAutoSyncKeepsIncrementalFetchWhenNamesExistOrNoContactIsStored(t *testing.T) {
	for _, tt := range []struct{ named, total int }{{5, 100}, {0, 0}} {
		backend := newFakeAppState()
		backend.named, backend.total = tt.named, tt.total
		h := newSyncHarness(backend)

		h.runOnce(t, false)

		if got, want := backend.fetchCalls(), callsFor(syncPatchNames, nil); !slices.Equal(got, want) {
			t.Errorf("named=%d total=%d: fetches = %v, want %v", tt.named, tt.total, got, want)
		}
	}
}

func TestManualResyncAlwaysRedownloadsTheAddressBook(t *testing.T) {
	backend := newFakeAppState()
	h := newSyncHarness(backend)

	h.runOnce(t, true)

	if got, want := backend.fetchCalls(), callsFor(syncPatchNames, map[appstate.WAPatchName]bool{appstate.WAPatchCriticalUnblockLow: true}); !slices.Equal(got, want) {
		t.Fatalf("fetches = %v, want %v", got, want)
	}
}

func TestSyncRetriesTransientErrorsWithBackoff(t *testing.T) {
	backend := newFakeAppState()
	backend.errs[appstate.WAPatchRegular] = []error{whatsmeow.ErrIQTimedOut, errors.New("server error 500")}
	h := newSyncHarness(backend)

	h.runOnce(t, false)

	n := 0
	for _, c := range backend.fetchCalls() {
		if c.name == appstate.WAPatchRegular {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("regular fetched %d times, want 3 (two retries)", n)
	}
	sleeps := h.sleptFor()
	base := defaultSyncTiming.retryBase
	if !slices.Contains(sleeps, base) || !slices.Contains(sleeps, 3*base) {
		t.Errorf("sleeps = %v, want backoff %v then %v", sleeps, base, 3*base)
	}
	if st := h.status(context.Background()); st.State != wa.ContactSyncDone {
		t.Errorf("state = %s, want done after a successful retry", st.State)
	}
}

func TestSyncGivesUpAPatchAfterMaxAttemptsAndContinues(t *testing.T) {
	backend := newFakeAppState()
	boom := errors.New("server error 500")
	backend.errs[appstate.WAPatchCriticalBlock] = []error{boom, boom, boom, boom}
	h := newSyncHarness(backend)

	h.runOnce(t, false)

	calls := backend.fetchCalls()
	if len(calls) != len(syncPatchNames)+defaultSyncTiming.maxAttempts-1 {
		t.Fatalf("fetches = %v, want %d attempts on critical_block and one fetch of every other patch", calls, defaultSyncTiming.maxAttempts)
	}
	st := h.status(context.Background())
	if st.State != wa.ContactSyncFailed {
		t.Errorf("state = %s, want failed", st.State)
	}
	for _, p := range st.Patches {
		wantResult := wa.PatchSynced
		if p.Name == string(appstate.WAPatchCriticalBlock) {
			wantResult = wa.PatchFailed
		}
		if p.LastResult != wantResult {
			t.Errorf("patch %s result = %s, want %s", p.Name, p.LastResult, wantResult)
		}
	}
}

func TestSyncReportsMissingKeysWithoutRetrying(t *testing.T) {
	backend := newFakeAppState()
	backend.errs[appstate.WAPatchCriticalUnblockLow] = []error{
		fmt.Errorf("failed to decode app state critical_unblock_low patches: %w", appstate.ErrKeyNotFound),
	}
	h := newSyncHarness(backend)

	h.runOnce(t, false)

	if got := backend.fetchCalls(); len(got) != len(syncPatchNames) {
		t.Fatalf("fetches = %v, want one per patch (no retry on missing keys)", got)
	}
	st := h.status(context.Background())
	if st.State != wa.ContactSyncKeysMissing {
		t.Errorf("state = %s, want keys_missing", st.State)
	}
	if p := st.Patches[0]; p.LastResult != wa.PatchKeysMissing || p.Error == "" {
		t.Errorf("address-book patch = %+v, want keys_missing with the error", p)
	}
}

func TestSyncStopsWhenTheConnectionDrops(t *testing.T) {
	backend := newFakeAppState()
	backend.errs[appstate.WAPatchCriticalBlock] = []error{whatsmeow.ErrNotConnected}
	h := newSyncHarness(backend)

	h.runOnce(t, false)

	if got, want := backend.fetchCalls(), callsFor(syncPatchNames[:2], nil); !slices.Equal(got, want) {
		t.Fatalf("fetches = %v, want to stop after %v", got, want)
	}
	st := h.status(context.Background())
	if st.State != wa.ContactSyncNotConnected {
		t.Errorf("state = %s, want not_connected", st.State)
	}
	for _, p := range st.Patches[2:] {
		if p.LastResult != wa.PatchSkipped {
			t.Errorf("patch %s = %s, want skipped", p.Name, p.LastResult)
		}
	}
}

func TestSyncStopsWhenLoggedOutMidRun(t *testing.T) {
	backend := newFakeAppState()
	backend.onFetch = func(name appstate.WAPatchName) {
		if name == appstate.WAPatchCriticalBlock {
			backend.mu.Lock()
			backend.loggedIn = false
			backend.mu.Unlock()
		}
	}
	h := newSyncHarness(backend)

	h.runOnce(t, false)

	if got := backend.fetchCalls(); len(got) != 2 {
		t.Fatalf("fetches = %v, want none after the session stopped being logged in", got)
	}
}

func TestSyncNeverFetchesWhenNotAuthenticated(t *testing.T) {
	backend := newFakeAppState()
	backend.loggedIn = false
	h := newSyncHarness(backend)

	h.runOnce(t, false)

	if got := backend.fetchCalls(); len(got) != 0 {
		t.Fatalf("fetches = %v, want none", got)
	}
	if st := h.status(context.Background()); st.State != wa.ContactSyncNotConnected {
		t.Errorf("state = %s, want not_connected", st.State)
	}
	select {
	case <-h.finished:
		t.Error("onFinished ran although nothing was synced")
	default:
	}
}

func TestARunThatNeverReachedWhatsAppDoesNotDelayTheNextOne(t *testing.T) {
	backend := newFakeAppState()
	backend.loggedIn = false
	h := newSyncHarness(backend)
	h.runOnce(t, false)

	backend.mu.Lock()
	backend.loggedIn = true
	backend.mu.Unlock()

	if outcome, ok := h.tryStart(); !ok {
		t.Fatalf("tryStart after a run that fetched nothing = %s, want started", outcome)
	}
}

func TestTryStartAllowsOneRunAtATimeAndSpacesRuns(t *testing.T) {
	h := newSyncHarness(newFakeAppState())

	if outcome, ok := h.tryStart(); !ok || outcome != wa.ResyncStarted {
		t.Fatalf("first tryStart = %s, %v", outcome, ok)
	}
	if outcome, ok := h.tryStart(); ok || outcome != wa.ResyncAlreadyRunning {
		t.Fatalf("tryStart while running = %s, %v; want already_running", outcome, ok)
	}
	h.run(context.Background(), false, 0)
	h.advance(defaultSyncTiming.minRunGap - time.Second)
	if outcome, ok := h.tryStart(); ok || outcome != wa.ResyncTooSoon {
		t.Fatalf("tryStart within the gap = %s, %v; want too_soon", outcome, ok)
	}
	h.advance(time.Second)
	if outcome, ok := h.tryStart(); !ok || outcome != wa.ResyncStarted {
		t.Fatalf("tryStart after the gap = %s, %v; want started", outcome, ok)
	}
}

func TestStartAutoWaitsBeforeFetchingAndRunsOncePerConnect(t *testing.T) {
	backend := newFakeAppState()
	h := newSyncHarness(backend)

	if !h.startAuto(context.Background()) {
		t.Fatal("startAuto did not start")
	}
	if h.startAuto(context.Background()) {
		t.Fatal("a second connect event started a second concurrent sync")
	}
	select {
	case <-h.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("sync did not finish")
	}

	if got := h.sleptFor(); len(got) == 0 || got[0] != defaultSyncTiming.startDelay {
		t.Errorf("sleeps = %v, want the start delay %v first", got, defaultSyncTiming.startDelay)
	}
	if got := backend.fetchCalls(); len(got) != len(syncPatchNames) {
		t.Errorf("fetches = %v, want one per patch", got)
	}
}

func TestStartManualRunsInTheBackgroundWithoutDelay(t *testing.T) {
	backend := newFakeAppState()
	h := newSyncHarness(backend)

	if got := h.startManual(context.Background()); got != wa.ResyncStarted {
		t.Fatalf("startManual = %s", got)
	}
	select {
	case <-h.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("sync did not finish")
	}
	if got := h.sleptFor(); slices.Contains(got, defaultSyncTiming.startDelay) {
		t.Errorf("sleeps = %v, a manual resync must not wait the post-connect delay", got)
	}
	if calls := backend.fetchCalls(); len(calls) == 0 || !calls[0].full {
		t.Errorf("fetches = %v, want a full address-book sync first", calls)
	}
}

func TestSyncStatusReportsStoredVersionsBeforeAnyRun(t *testing.T) {
	backend := newFakeAppState()
	backend.versions = map[appstate.WAPatchName]uint64{appstate.WAPatchCriticalBlock: 4, appstate.WAPatchRegularLow: 104}
	backend.named, backend.total = 0, 1164
	h := newSyncHarness(backend)

	st := h.status(context.Background())

	if st.State != wa.ContactSyncNotRun || st.ContactsNamed != 0 || st.ContactsTotal != 1164 {
		t.Fatalf("status = %+v", st)
	}
	versions := map[string]uint64{}
	for _, p := range st.Patches {
		versions[p.Name] = p.Version
		if p.LastResult != "" {
			t.Errorf("patch %s has a result before any run: %+v", p.Name, p)
		}
	}
	want := map[string]uint64{"critical_unblock_low": 0, "critical_block": 4, "regular_high": 0, "regular": 0, "regular_low": 104}
	if fmt.Sprint(versions) != fmt.Sprint(want) {
		t.Errorf("versions = %v, want %v", versions, want)
	}
}

func TestClassifyFetchError(t *testing.T) {
	tests := []struct {
		err  error
		want fetchErrorKind
	}{
		{fmt.Errorf("decode: %w", appstate.ErrKeyNotFound), fetchKeysMissing},
		{whatsmeow.ErrNotConnected, fetchDisconnected},
		{fmt.Errorf("x: %w", whatsmeow.ErrNotLoggedIn), fetchDisconnected},
		{whatsmeow.ErrIQDisconnected, fetchDisconnected},
		{context.Canceled, fetchDisconnected},
		{whatsmeow.ErrIQTimedOut, fetchTransient},
		{appstate.ErrMismatchingLTHash, fetchTransient},
	}
	for _, tt := range tests {
		if got := classifyFetchError(tt.err); got != tt.want {
			t.Errorf("classifyFetchError(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}
