package wameow

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

func lookupFrom(m map[types.JID]types.JID) func(types.JID) (types.JID, bool) {
	return func(j types.JID) (types.JID, bool) {
		v, ok := m[j]
		return v, ok
	}
}

func TestMergeContacts(t *testing.T) {
	pnCamille := types.NewJID("33677000099", types.DefaultUserServer)
	lidCamille := types.NewJID("123456789012345", types.HiddenUserServer)
	lidOnly := types.NewJID("99999999999999", types.HiddenUserServer)
	pnToLID := map[types.JID]types.JID{pnCamille: lidCamille, pnAlice: lidAlice}
	lidToPN := map[types.JID]types.JID{lidCamille: pnCamille, lidAlice: pnAlice}

	all := map[types.JID]types.ContactInfo{
		pnCamille:                {FullName: "Camille Martin", FirstName: "Camille"},
		lidCamille:               {PushName: "Cami"},
		pnAlice:                  {FullName: "Alice Saved", PushName: "pn push"},
		lidAlice:                 {FullName: "Alice From Lid", PushName: "lid push", BusinessName: "Alice SARL"},
		withDevice(lidOnly, 3):   {PushName: "Lid Only"},
		pnBob:                    {},
		group:                    {FullName: "A group"},
		types.StatusBroadcastJID: {PushName: "status"},
	}

	got := mergeContacts(all, lookupFrom(lidToPN), lookupFrom(pnToLID))

	want := []wa.Contact{
		{JID: pnAlice.String(), Name: "Alice Saved", FirstName: "", BusinessName: "Alice SARL", PushName: "pn push",
			PN: pnAlice.String(), LID: lidAlice.String()},
		{JID: pnCamille.String(), Name: "Camille Martin", FirstName: "Camille", PushName: "Cami",
			PN: pnCamille.String(), LID: lidCamille.String()},
		{JID: lidOnly.String(), PushName: "Lid Only", LID: lidOnly.String()},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("mergeContacts =\n %+v\nwant\n %+v", got, want)
	}
}

func TestMergeContactsMergesALIDRowWhoseMappingIsOnlyKnownFromThePNSide(t *testing.T) {
	pn := types.NewJID("33677000099", types.DefaultUserServer)
	lid := types.NewJID("123456789012345", types.HiddenUserServer)
	all := map[types.JID]types.ContactInfo{pn: {FullName: "Camille Martin"}, lid: {PushName: "Cami"}}

	got := mergeContacts(all, lookupFrom(map[types.JID]types.JID{lid: pn}), lookupFrom(nil))

	if len(got) != 1 || got[0].JID != pn.String() || got[0].LID != lid.String() || got[0].PushName != "Cami" {
		t.Fatalf("mergeContacts = %+v, want one merged contact", got)
	}
}

// pairedTestClient opens a client whose store holds a device identity, so the
// contact and LID stores work, without ever connecting.
func pairedTestClient(t *testing.T) *Client {
	t.Helper()
	ctx := context.Background()
	c, err := Open(ctx, filepath.Join(t.TempDir(), "whatsapp.db"), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	own := types.NewADJID("33699999999", 0, 1)
	c.cli.Store.ID = &own
	c.cli.Store.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{1}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := c.cli.Store.Save(ctx); err != nil {
		t.Fatalf("save device: %v", err)
	}
	return c
}

func TestOpenAsksWhatsmeowForContactEventsOnFullSync(t *testing.T) {
	c, err := Open(context.Background(), filepath.Join(t.TempDir(), "whatsapp.db"), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if !c.cli.EmitAppStateEventsOnFullSync {
		t.Fatal("EmitAppStateEventsOnFullSync = false; full syncs would hide the LID/phone pairs of address-book entries")
	}
}

func TestUnpairedClientRefusesResyncAndStatus(t *testing.T) {
	c, err := Open(context.Background(), filepath.Join(t.TempDir(), "whatsapp.db"), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if _, err := c.ResyncContacts(context.Background()); !errors.Is(err, wa.ErrNotPaired) {
		t.Errorf("ResyncContacts = %v, want ErrNotPaired", err)
	}
	if _, err := c.ContactSync(context.Background()); !errors.Is(err, wa.ErrNotPaired) {
		t.Errorf("ContactSync = %v, want ErrNotPaired", err)
	}
}

func TestDisconnectedClientRefusesResyncWithoutStartingOne(t *testing.T) {
	c := pairedTestClient(t)

	if _, err := c.ResyncContacts(context.Background()); !errors.Is(err, wa.ErrNotConnected) {
		t.Fatalf("ResyncContacts = %v, want ErrNotConnected", err)
	}
	st, err := c.ContactSync(context.Background())
	if err != nil {
		t.Fatalf("ContactSync: %v", err)
	}
	if st.State != wa.ContactSyncNotRun || len(st.Patches) != len(syncPatchNames) {
		t.Fatalf("status = %+v, want not_run with every patch listed", st)
	}
	if (whatsmeowAppState{c: c}).LoggedIn() {
		t.Fatal("a never-connected client reports logged in")
	}
}

func TestContactActionRecordsTheLIDPhoneMapping(t *testing.T) {
	c := pairedTestClient(t)
	ctx := context.Background()
	pn := types.NewJID("33677000099", types.DefaultUserServer)
	lid := types.NewJID("123456789012345", types.HiddenUserServer)

	c.handleEvent(&events.Contact{JID: pn, Action: &waSyncAction.ContactAction{
		FullName: proto.String("Camille Martin"), LidJID: proto.String(lid.String()),
	}})

	got, err := c.cli.Store.LIDs.GetPNForLID(ctx, lid)
	if err != nil || got != pn {
		t.Fatalf("GetPNForLID = %v, %v; want %v", got, err, pn)
	}
}

func TestAppStateEventsAreCountedAsMutations(t *testing.T) {
	c := pairedTestClient(t)

	c.handleEvent(&events.AppState{})
	c.handleEvent(&events.AppState{})

	if got := c.appStateMutations.Load(); got != 2 {
		t.Fatalf("mutations = %d, want 2", got)
	}
}

func TestContactsMergeStoredAddressBookNamesThroughTheLIDMap(t *testing.T) {
	c := pairedTestClient(t)
	ctx := context.Background()
	pn := types.NewJID("33677000099", types.DefaultUserServer)
	lid := types.NewJID("123456789012345", types.HiddenUserServer)
	if err := c.cli.Store.Contacts.PutContactName(ctx, pn, "Camille", "Camille Martin"); err != nil {
		t.Fatalf("PutContactName: %v", err)
	}
	if _, _, err := c.cli.Store.Contacts.PutPushName(ctx, lid, "Cami"); err != nil {
		t.Fatalf("PutPushName: %v", err)
	}
	c.cli.StoreLIDPNMapping(ctx, lid, pn)

	got, err := c.Contacts(ctx)
	if err != nil {
		t.Fatalf("Contacts: %v", err)
	}
	want := []wa.Contact{{JID: pn.String(), Name: "Camille Martin", FirstName: "Camille", PushName: "Cami", PN: pn.String(), LID: lid.String()}}
	if !slices.Equal(got, want) {
		t.Fatalf("Contacts = %+v, want %+v", got, want)
	}
	named, total, err := (whatsmeowAppState{c: c}).ContactCounts(ctx)
	if err != nil || named != 1 || total != 2 {
		t.Fatalf("ContactCounts = %d/%d, %v; want 1 named of 2", named, total, err)
	}
	version, err := (whatsmeowAppState{c: c}).PatchVersion(ctx, appstate.WAPatchCriticalUnblockLow)
	if err != nil || version != 0 {
		t.Fatalf("PatchVersion = %d, %v; want 0 for a never-synced collection", version, err)
	}
}

func TestConnectedEventStartsTheAppStateSync(t *testing.T) {
	c := pairedTestClient(t)
	c.sync.timing.startDelay = time.Hour

	c.handleEvent(&events.Connected{})

	if outcome, ok := c.sync.tryStart(); ok || outcome != wa.ResyncAlreadyRunning {
		t.Fatalf("tryStart after Connected = %s, %v; want the post-connect sync already running", outcome, ok)
	}
}
