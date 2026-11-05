package tools_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

const (
	camillePN  = "33677000099@s.whatsapp.net"
	camilleLID = "123456789012345@lid"
	anaisJID   = "33612121212@s.whatsapp.net"
	jpJID      = "33613131313@s.whatsapp.net"
	bakeryJID  = "33614141414@s.whatsapp.net"
	mimiJID    = "33615151515@s.whatsapp.net"
	zoeLID     = "88888888888888@lid"
	yvesPN     = "33612345678@s.whatsapp.net"
	yvesLID    = "77777777777777@lid"
)

type fullContactsOut struct {
	Contacts []struct {
		JID      string `json:"jid"`
		Name     string `json:"name"`
		PushName string `json:"push_name"`
		Phone    string `json:"phone"`
		PN       string `json:"pn"`
		LID      string `json:"lid"`
	} `json:"contacts"`
}

func searchJIDs(e *env, query string) []string {
	e.t.Helper()
	got := callAs[fullContactsOut](e, "search_contacts", map[string]any{"query": query})
	out := make([]string, 0, len(got.Contacts))
	for _, c := range got.Contacts {
		out = append(out, c.JID)
	}
	return out
}

func addressBook() []wa.Contact {
	return []wa.Contact{
		{JID: camillePN, Name: "Camille Martin", FirstName: "Camille", PN: camillePN, LID: camilleLID},
		{JID: anaisJID, Name: "Anaïs Durand", PN: anaisJID},
		{JID: jpJID, Name: "Jean-Pierre Lefèvre", PN: jpJID},
		{JID: bakeryJID, BusinessName: "Boulangerie Émile", PushName: "emile", PN: bakeryJID},
		{JID: mimiJID, PushName: "Mimi", PN: mimiJID},
		{JID: zoeLID, PushName: "Zoé", LID: zoeLID},
		{JID: yvesPN, PN: yvesPN, LID: yvesLID, PushName: "Yves"},
	}
}

func TestSearchContactsMatching(t *testing.T) {
	e := newEnv(t)
	e.fake.SetContacts(addressBook())

	tests := []struct {
		query string
		want  []string
	}{
		{"camille", []string{camillePN}},
		{"martin", []string{camillePN}},
		{"camille martin", []string{camillePN}},
		{"martin camille", []string{camillePN}},
		{"  MARTIN   Camille ", []string{camillePN}},
		{"camille durand", nil},
		{"anais", []string{anaisJID}},
		{"ANAÏS", []string{anaisJID}},
		{"durand ana", []string{anaisJID}},
		{"jean pierre", []string{jpJID}},
		{"lefevre", []string{jpJID}},
		{"boulangerie", []string{bakeryJID}},
		{"emile", []string{bakeryJID}},
		{"mimi", []string{mimiJID}},
		{"zoe", []string{zoeLID}},
		{"06 77 00 00 99", []string{camillePN}},
		{"0677000099", []string{camillePN}},
		{"+33 6 77 00 00 99", []string{camillePN}},
		{"+33677000099", []string{camillePN}},
		{"0033677000099", []string{camillePN}},
		{"33677000099", []string{camillePN}},
		{"00 00 99", []string{camillePN}},
		{"camille 0677", []string{camillePN}},
		{"123456789", []string{camillePN}},
		{"88888888", []string{zoeLID}},
		{"06 12 34 56 78", []string{yvesPN}},
		{"8888", []string{zoeLID}},
		{"nobody", nil},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			if got := searchJIDs(e, tt.query); !slices.Equal(got, tt.want) {
				t.Fatalf("search %q = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestSearchContactsLIDMatchesOnlyFromTheStartOfTheNumber(t *testing.T) {
	e := newEnv(t)
	e.fake.SetContacts(addressBook())

	if got := searchJIDs(e, "456789012"); slices.Contains(got, camillePN) {
		t.Fatalf("search = %v: a fragment inside a LID must not match", got)
	}
}

func TestSearchContactsRanksExactThenPrefixThenSubstringThenPushName(t *testing.T) {
	e := newEnv(t)
	e.fake.SetContacts([]wa.Contact{
		{JID: "33600000001@s.whatsapp.net", PushName: "marie"},
		{JID: "33600000002@s.whatsapp.net", Name: "Rosemarie Roux"},
		{JID: "33600000003@s.whatsapp.net", Name: "Marie-Claire Dupont"},
		{JID: "33600000004@s.whatsapp.net", Name: "Marie"},
		{JID: "33600000005@s.whatsapp.net", Name: "Annemarie Blanc"},
		{JID: "33600000006@s.whatsapp.net", Name: "Dupont Marie", PushName: "md"},
	})

	got := callAs[fullContactsOut](e, "search_contacts", map[string]any{"query": "Marie"})
	names := make([]string, 0, len(got.Contacts))
	for _, c := range got.Contacts {
		names = append(names, cmpOr(c.Name, "push:"+c.PushName))
	}
	want := []string{"Marie", "Dupont Marie", "Marie-Claire Dupont", "Annemarie Blanc", "Rosemarie Roux", "push:marie"}
	if !slices.Equal(names, want) {
		t.Fatalf("order = %v, want %v", names, want)
	}

	exact := callAs[fullContactsOut](e, "search_contacts", map[string]any{"query": "marie dupont"})
	if len(exact.Contacts) != 2 || exact.Contacts[0].Name != "Dupont Marie" {
		t.Fatalf("query \"marie dupont\" = %+v, want the exact word match first", exact.Contacts)
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func TestSearchContactsMergesTheLIDAndPhoneNumberEntriesOfOnePerson(t *testing.T) {
	e := newEnv(t)
	e.fake.SetContacts([]wa.Contact{
		{JID: camilleLID, PushName: "Cami", LID: camilleLID},
		{JID: camillePN, Name: "Camille Martin", PN: camillePN, LID: camilleLID},
	})
	e.seedMessages(wa.Message{ID: "l1", ChatJID: camilleLID, SenderJID: camilleLID, Timestamp: epoch, Text: "salut"})

	got := callAs[fullContactsOut](e, "search_contacts", map[string]any{"query": "mi"})

	if len(got.Contacts) != 1 {
		t.Fatalf("contacts = %+v, want the person once", got.Contacts)
	}
	c := got.Contacts[0]
	if c.JID != camillePN || c.Name != "Camille Martin" || c.PushName != "Cami" || c.Phone != "33677000099" ||
		c.PN != camillePN || c.LID != camilleLID {
		t.Fatalf("contact = %+v", c)
	}
}

func TestSavedContactWithoutPushNameAndWithALIDChatIsFoundEverywhere(t *testing.T) {
	e := newEnv(t)
	e.fake.SetContacts([]wa.Contact{
		{JID: camillePN, Name: "Camille Martin", FirstName: "Camille", PN: camillePN, LID: camilleLID},
		{JID: yvesPN, PN: yvesPN, LID: yvesLID},
	})
	e.seedMessages(
		wa.Message{ID: "m1", ChatJID: camilleLID, SenderJID: camilleLID, Timestamp: epoch, Text: "on se voit ?"},
		wa.Message{ID: "y1", ChatJID: yvesLID, SenderJID: yvesLID, Timestamp: epoch.Add(time.Minute), Text: "ok"},
	)

	for _, q := range []string{"camille", "martin", "Camille Martin", "06 77 00 00 99"} {
		if got := searchJIDs(e, q); !slices.Equal(got, []string{camillePN}) {
			t.Errorf("search_contacts %q = %v, want [%s]", q, got, camillePN)
		}
		chats := callAs[chatsOut](e, "list_chats", map[string]any{"query": q})
		if len(chats.Chats) != 1 || chats.Chats[0].JID != camilleLID || chats.Chats[0].Name != "Camille Martin" {
			t.Errorf("list_chats %q = %+v, want the LID chat named after the address book", q, chats.Chats)
		}
	}

	if got := searchJIDs(e, "+33 6 12 34 56 78"); !slices.Equal(got, []string{yvesPN}) {
		t.Errorf("search by number of a LID-only chat = %v, want [%s]", got, yvesPN)
	}
	chats := callAs[chatsOut](e, "list_chats", map[string]any{"query": "0612345678"})
	if len(chats.Chats) != 1 || chats.Chats[0].JID != yvesLID || chats.Chats[0].Name != "+33612345678" {
		t.Errorf("list_chats by number = %+v, want the LID chat titled with its number", chats.Chats)
	}
}

func TestListChatsNameFallsBackToAddressBookPushNameThenNumber(t *testing.T) {
	e := newEnv(t)
	const (
		storedPN  = "33601010101@s.whatsapp.net"
		savedPN   = "33602020202@s.whatsapp.net"
		savedLID  = "22222222222222@lid"
		savedLPN  = "33603030303@s.whatsapp.net"
		pushPN    = "33604040404@s.whatsapp.net"
		unknownPN = "33605050505@s.whatsapp.net"
		unknownLD = "33333333333333@lid"
		bizPN     = "33606060606@s.whatsapp.net"
		stalePN   = "33607070707@s.whatsapp.net"
	)
	e.fake.SetContacts([]wa.Contact{
		{JID: storedPN, PushName: "push only", PN: storedPN},
		{JID: stalePN, Name: "Real Saved", PushName: "stale push", PN: stalePN},
		{JID: savedPN, Name: "Bob Saved", PushName: "bobby", PN: savedPN},
		{JID: savedLPN, Name: "Carol Saved", PN: savedLPN, LID: savedLID},
		{JID: pushPN, PushName: "Only Push", PN: pushPN},
		{JID: bizPN, BusinessName: "ACME", PushName: "acme", PN: bizPN},
	})
	e.seedChats(wa.Chat{JID: storedPN, Name: "Stored Name"}, wa.Chat{JID: stalePN, Name: "stale push"})
	at := epoch
	for _, jid := range []string{storedPN, stalePN, savedPN, savedLID, pushPN, unknownPN, unknownLD, bizPN, groupJID} {
		at = at.Add(time.Minute)
		e.seedMessages(wa.Message{ID: "x" + jid, ChatJID: jid, SenderJID: jid, Timestamp: at, Text: "hi"})
	}

	got := callAs[chatsOut](e, "list_chats", map[string]any{"limit": 50})

	names := map[string]string{}
	for _, c := range got.Chats {
		names[c.JID] = c.Name
	}
	want := map[string]string{
		storedPN:  "Stored Name",
		stalePN:   "Real Saved",
		savedPN:   "Bob Saved",
		savedLID:  "Carol Saved",
		pushPN:    "Only Push",
		unknownPN: "+33605050505",
		unknownLD: "",
		bizPN:     "ACME",
		groupJID:  "",
	}
	for jid, name := range want {
		if names[jid] != name {
			t.Errorf("chat %s name = %q, want %q", jid, names[jid], name)
		}
	}
}

func TestListChatsQueryPaginatesResolvedMatches(t *testing.T) {
	e := newEnv(t)
	e.fake.SetContacts(addressBook())
	for i, jid := range []string{camilleLID, anaisJID, jpJID, mimiJID} {
		e.seedMessages(wa.Message{ID: "p" + jid, ChatJID: jid, SenderJID: jid, Timestamp: epoch.Add(time.Duration(i) * time.Minute), Text: "x"})
	}

	first := callAs[chatsOut](e, "list_chats", map[string]any{"query": "mi", "limit": 1})
	second := callAs[chatsOut](e, "list_chats", map[string]any{"query": "mi", "limit": 1, "offset": first.NextOffset})

	if len(first.Chats) != 1 || !first.HasMore || first.Chats[0].JID != mimiJID {
		t.Fatalf("first page = %+v", first)
	}
	if len(second.Chats) != 1 || second.HasMore || second.Chats[0].JID != camilleLID {
		t.Fatalf("second page = %+v", second)
	}
}

func TestListChatsQueryStillMatchesTheJID(t *testing.T) {
	e := newEnv(t)
	e.seedMessages(wa.Message{ID: "g1", ChatJID: groupJID, SenderJID: aliceJID, Timestamp: epoch, Text: "x"})

	got := callAs[chatsOut](e, "list_chats", map[string]any{"query": "120363"})

	if len(got.Chats) != 1 || got.Chats[0].JID != groupJID {
		t.Fatalf("chats = %+v", got.Chats)
	}
}

func TestMessageSenderNamePrefersTheAddressBook(t *testing.T) {
	e := newEnv(t)
	e.fake.SetContacts([]wa.Contact{
		{JID: camillePN, Name: "Camille Martin", PN: camillePN, LID: camilleLID},
		{JID: mimiJID, PushName: "Mimi", PN: mimiJID},
	})
	e.seedMessages(
		wa.Message{ID: "s1", ChatJID: groupJID, SenderJID: camilleLID, SenderName: "Cami", Timestamp: epoch, Text: "a"},
		wa.Message{ID: "s2", ChatJID: groupJID, SenderJID: mimiJID, Timestamp: epoch.Add(time.Minute), Text: "b"},
		wa.Message{ID: "s3", ChatJID: groupJID, SenderJID: "33609090909@s.whatsapp.net", Timestamp: epoch.Add(2 * time.Minute), Text: "c"},
		wa.Message{ID: "s4", ChatJID: groupJID, SenderJID: "44444444444444@lid", SenderName: "Stranger", Timestamp: epoch.Add(3 * time.Minute), Text: "d"},
		wa.Message{ID: "s5", ChatJID: groupJID, SenderJID: selfJID, FromMe: true, Timestamp: epoch.Add(4 * time.Minute), Text: "e"},
	)

	got := callAs[msgsOut](e, "list_messages", map[string]any{"chat_jid": groupJID, "oldest_first": true})

	names := make([]string, 0, len(got.Messages))
	for _, m := range got.Messages {
		names = append(names, m.SenderName)
	}
	if want := []string{"Camille Martin", "Mimi", "+33609090909", "Stranger", ""}; !slices.Equal(names, want) {
		t.Fatalf("sender names = %q, want %q", names, want)
	}

	ctxOut := callAs[contextOut](e, "get_message_context", map[string]any{"message_id": "s1", "chat_jid": groupJID})
	if ctxOut.Message.SenderName != "Camille Martin" {
		t.Fatalf("get_message_context sender_name = %q", ctxOut.Message.SenderName)
	}
}

func TestMessageSenderNameKeepsStoredNameWhenContactsAreUnavailable(t *testing.T) {
	e := newEnv(t, withStatus(wa.Status{State: wa.StateNeedsQR}))
	e.seedMessages(wa.Message{ID: "s1", ChatJID: aliceJID, SenderJID: aliceJID, SenderName: "Alice", Timestamp: epoch, Text: "a"})

	got := callAs[msgsOut](e, "list_messages", map[string]any{})

	if len(got.Messages) != 1 || got.Messages[0].SenderName != "Alice" {
		t.Fatalf("messages = %+v", got.Messages)
	}
}

type resyncOut struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

func TestResyncContactsStartsASyncAndSendsNothing(t *testing.T) {
	e := newEnv(t)

	got := callAs[resyncOut](e, "resync_contacts", map[string]any{})

	if got.Status != "started" || !strings.Contains(got.Message, "No chat message was sent") {
		t.Fatalf("resync_contacts = %+v", got)
	}
	if n := e.fake.ResyncCalls(); n != 1 {
		t.Fatalf("ResyncContacts called %d times, want 1", n)
	}
	e.assertNothingSent()
}

func TestResyncContactsReportsBusyAndTooSoon(t *testing.T) {
	for _, outcome := range []wa.ResyncOutcome{wa.ResyncAlreadyRunning, wa.ResyncTooSoon} {
		e := newEnv(t)
		e.fake.SetResync(outcome, nil)

		got := callAs[resyncOut](e, "resync_contacts", map[string]any{})

		if got.Status != string(outcome) || got.Message == "" {
			t.Errorf("outcome %s: resync_contacts = %+v", outcome, got)
		}
		e.assertNothingSent()
	}
}

func TestResyncContactsFailsWhenNotConnected(t *testing.T) {
	e := newEnv(t)
	e.fake.SetResync("", wa.ErrNotConnected)

	e.fail("resync_contacts", map[string]any{}, "not connected")
	e.assertNothingSent()
}

func TestResyncContactsIsAnnotatedNonDestructive(t *testing.T) {
	tl := listTools(t, newEnv(t))["resync_contacts"]
	if tl == nil {
		t.Fatal("resync_contacts is not registered")
	}
	a := tl.Annotations
	if a == nil || a.DestructiveHint == nil || *a.DestructiveHint {
		t.Fatalf("annotations = %+v, want non-destructive", a)
	}
	if !strings.Contains(tl.Description, "does not send any chat message") {
		t.Fatalf("description = %q, must state that no message is sent", tl.Description)
	}
	if strings.Contains(tl.Description, sendWarning) {
		t.Fatal("resync_contacts must not carry the send warning")
	}
}

func TestResyncContactsIsAvailableWithSendingDisabled(t *testing.T) {
	e := newEnv(t, withSendDisabled)

	if got := callAs[resyncOut](e, "resync_contacts", map[string]any{}); got.Status != "started" {
		t.Fatalf("resync_contacts = %+v", got)
	}
}

type syncStatusOut struct {
	ContactSync *struct {
		State            string `json:"state"`
		ContactsTotal    int    `json:"contacts_total"`
		ContactsWithName int    `json:"contacts_with_name"`
		LastFinished     string `json:"last_finished"`
		Hint             string `json:"hint"`
		Patches          []struct {
			Name       string `json:"name"`
			Version    uint64 `json:"version"`
			LastResult string `json:"last_result"`
			Error      string `json:"error"`
		} `json:"patches"`
	} `json:"contact_sync"`
}

func TestConnectionStatusReportsAddressBookCounts(t *testing.T) {
	e := newEnv(t)
	e.fake.SetContactSync(wa.ContactSyncStatus{
		State: wa.ContactSyncDone, ContactsTotal: 1164, ContactsNamed: 1020,
		LastFinished: epoch,
		Patches: []wa.PatchSync{
			{Name: "critical_unblock_low", Version: 12, LastResult: wa.PatchSynced},
			{Name: "critical_block", Version: 4, LastResult: wa.PatchSynced},
		},
	})

	got := callAs[syncStatusOut](e, "connection_status", map[string]any{})

	cs := got.ContactSync
	if cs == nil || cs.State != "done" || cs.ContactsTotal != 1164 || cs.ContactsWithName != 1020 ||
		cs.LastFinished != "2025-03-01T12:00:00Z" || cs.Hint != "" || len(cs.Patches) != 2 ||
		cs.Patches[0].Name != "critical_unblock_low" || cs.Patches[0].Version != 12 {
		t.Fatalf("contact_sync = %+v", cs)
	}
}

func TestConnectionStatusHints(t *testing.T) {
	tests := []struct {
		name string
		st   wa.ContactSyncStatus
		want string
	}{
		{"no saved names", wa.ContactSyncStatus{State: wa.ContactSyncDone, ContactsTotal: 1164}, "resync_contacts"},
		{"keys missing", wa.ContactSyncStatus{State: wa.ContactSyncKeysMissing, ContactsTotal: 1164,
			Patches: []wa.PatchSync{{Name: "critical_unblock_low", LastResult: wa.PatchKeysMissing, Error: "didn't find app state key"}}}, "pair again"},
		{"running", wa.ContactSyncStatus{State: wa.ContactSyncRunning, ContactsTotal: 1164}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.fake.SetContactSync(tt.st)

			got := callAs[syncStatusOut](e, "connection_status", map[string]any{})

			if got.ContactSync == nil {
				t.Fatal("contact_sync missing")
			}
			if tt.want == "" && got.ContactSync.Hint != "" || !strings.Contains(got.ContactSync.Hint, tt.want) {
				t.Fatalf("hint = %q, want it to contain %q", got.ContactSync.Hint, tt.want)
			}
		})
	}
}

func TestConnectionStatusOmitsContactSyncWhenUnpaired(t *testing.T) {
	e := newEnv(t, withStatus(wa.Status{State: wa.StateNeedsQR}))

	raw := rawJSON(t, e.ok("connection_status", map[string]any{}).StructuredContent)

	if strings.Contains(raw, "contact_sync") {
		t.Fatalf("connection_status = %s, want no contact_sync before pairing", raw)
	}
}

func TestSendToASavedNameResolvesOncePerPersonAcrossLIDAndPhoneNumber(t *testing.T) {
	e := newEnv(t)
	e.fake.SetContacts([]wa.Contact{{JID: camillePN, Name: "Camille Martin", PN: camillePN, LID: camilleLID}})
	e.seedChats(wa.Chat{JID: camilleLID, Name: "Camille Martin"})

	res := callAs[map[string]any](e, "send_message", map[string]any{"recipient": "camille martin", "text": "salut"})

	if res["status"] != "sent" || res["chat_jid"] != camillePN {
		t.Fatalf("send_message = %v, want sent to %s", res, camillePN)
	}
	if sent := e.fake.Sent(); len(sent) != 1 || sent[0].ChatJID != camillePN {
		t.Fatalf("sent = %+v", sent)
	}
}

func TestSendNeverAutoResolvesABusinessName(t *testing.T) {
	e := newEnv(t)
	e.fake.SetContacts([]wa.Contact{{JID: bakeryJID, BusinessName: "Boulangerie", PN: bakeryJID}})

	e.fail("send_message", map[string]any{"recipient": "Boulangerie", "text": "salut"}, "no contact or chat matches")
	e.assertNothingSent()
}
