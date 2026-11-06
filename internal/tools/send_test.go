package tools_test

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/store"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

type sendResult struct {
	Status     string `json:"status"`
	MessageID  string `json:"message_id"`
	ChatJID    string `json:"chat_jid"`
	Timestamp  string `json:"timestamp"`
	Candidates []struct {
		JID    string `json:"jid"`
		Name   string `json:"name"`
		Source string `json:"source"`
	} `json:"candidates"`
	Note string `json:"note"`
}

func seedAddressBook(e *env) {
	e.fake.SetContacts([]wa.Contact{
		{JID: aliceJID, Name: "Alice Smith", PushName: "Ali"},
		{JID: bobJID, Name: "Bob Builder", PushName: "Bobby"},
	})
	e.seedChats(wa.Chat{JID: carolJID, Name: "Carol Danvers"}, wa.Chat{JID: groupJID, Name: "Family", IsGroup: true})
}

func TestSendMessageToResolvedRecipients(t *testing.T) {
	tests := []struct {
		name      string
		recipient string
		wantJID   string
	}{
		{"user jid", aliceJID, aliceJID},
		{"group jid", groupJID, groupJID},
		{"lid jid", "123456789@lid", "123456789@lid"},
		{"plus international phone", "+33 6 11 11 11 11", aliceJID},
		{"double zero phone", "0033611111111", aliceJID},
		{"punctuated phone", "+33-6.11.11.11.11", aliceJID},
		{"phone of unknown person", "+44 7700 900123", "447700900123@s.whatsapp.net"},
		{"exact contact name", "Alice Smith", aliceJID},
		{"exact name ignoring case", "alice SMITH", aliceJID},
		{"name with surrounding spaces", "  Bob Builder  ", bobJID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			seedAddressBook(e)

			got := callAs[sendResult](e, "send_message", map[string]any{"recipient": tt.recipient, "text": "hello there"})

			if got.Status != "sent" || got.ChatJID != tt.wantJID || got.MessageID != "FAKE000001" ||
				got.Timestamp != epoch.Format(time.RFC3339) {
				t.Fatalf("result = %+v", got)
			}
			sent := e.fake.Sent()
			if len(sent) != 1 || sent[0].ChatJID != tt.wantJID || sent[0].Text != "hello there" || sent[0].File != nil {
				t.Fatalf("sent = %+v", sent)
			}
		})
	}
}

func TestSendMessageSavesOutgoingMessageToStore(t *testing.T) {
	e := newEnv(t)
	seedAddressBook(e)

	got := callAs[sendResult](e, "send_message", map[string]any{"recipient": aliceJID, "text": "stored body"})

	saved, err := e.store.GetMessage(e.ctx(), aliceJID, got.MessageID)
	if err != nil {
		t.Fatalf("sent message not stored: %v", err)
	}
	if !saved.FromMe || saved.Text != "stored body" || saved.SenderJID != selfJID || !saved.Timestamp.Equal(epoch) || saved.Media != nil {
		t.Fatalf("stored = %+v", saved)
	}
	chat, err := e.store.GetChat(e.ctx(), aliceJID)
	if err != nil || chat.LastMessageText != "stored body" || !chat.LastMessageFromMe {
		t.Fatalf("chat = %+v err=%v", chat, err)
	}
}

func TestSendMessageKeepsTextVerbatim(t *testing.T) {
	e := newEnv(t, withRate(0, 1000))
	texts := []string{"  leading and trailing  ", "line1\nline2", "emoji 😀 and ünïcödé", "'; DROP TABLE messages; --", "<b>html</b> & %_"}

	for _, text := range texts {
		e.ok("send_message", map[string]any{"recipient": aliceJID, "text": text})
	}

	sent := e.fake.Sent()
	if len(sent) != len(texts) {
		t.Fatalf("sent %d, want %d", len(sent), len(texts))
	}
	for i, text := range texts {
		if sent[i].Text != text {
			t.Errorf("sent[%d] = %q, want %q", i, sent[i].Text, text)
		}
	}
	if _, err := e.store.GetChat(e.ctx(), aliceJID); err != nil {
		t.Fatalf("store damaged by hostile text: %v", err)
	}
}

func TestSendMessageAmbiguousRecipientSendsNothing(t *testing.T) {
	tests := []struct {
		name       string
		contacts   []wa.Contact
		chats      []wa.Chat
		recipient  string
		wantJIDs   []string
		wantStatus string
	}{
		{
			name: "two contacts with the same name",
			contacts: []wa.Contact{
				{JID: aliceJID, Name: "Alex"},
				{JID: bobJID, Name: "Alex"},
			},
			recipient: "Alex",
			wantJIDs:  []string{aliceJID, bobJID},
		},
		{
			name: "name matches contact name of one and push name of another",
			contacts: []wa.Contact{
				{JID: aliceJID, Name: "Sam"},
				{JID: bobJID, Name: "Robert", PushName: "sam"},
			},
			recipient: "SAM",
			wantJIDs:  []string{aliceJID, bobJID},
		},
		{
			name:      "contact and unrelated chat share the exact name",
			contacts:  []wa.Contact{{JID: aliceJID, Name: "Kim"}},
			chats:     []wa.Chat{{JID: carolJID, Name: "Kim"}},
			recipient: "kim",
			wantJIDs:  []string{aliceJID, carolJID},
		},
		{
			name:      "partial match only",
			contacts:  []wa.Contact{{JID: aliceJID, Name: "Alice Smith"}},
			recipient: "Ali",
			wantJIDs:  []string{aliceJID},
		},
		{
			name: "partial matches only, several",
			contacts: []wa.Contact{
				{JID: aliceJID, Name: "Alexander"},
				{JID: bobJID, Name: "Alexandra", PushName: "Lex"},
			},
			chats:     []wa.Chat{{JID: carolJID, Name: "Alexis"}},
			recipient: "Alex",
			wantJIDs:  []string{aliceJID, bobJID, carolJID},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.fake.SetContacts(tt.contacts)
			e.seedChats(tt.chats...)

			got := callAs[sendResult](e, "send_message", map[string]any{"recipient": tt.recipient, "text": "hi"})

			if got.Status != "ambiguous" || got.MessageID != "" || got.ChatJID != "" {
				t.Fatalf("result = %+v, want a not-sent ambiguous result", got)
			}
			if !strings.Contains(strings.ToLower(got.Note), "nothing was sent") {
				t.Fatalf("note = %q", got.Note)
			}
			var jids []string
			for _, c := range got.Candidates {
				jids = append(jids, c.JID)
				if c.Name == "" {
					t.Errorf("candidate %s has no name", c.JID)
				}
			}
			if strings.Join(jids, ",") != strings.Join(tt.wantJIDs, ",") {
				t.Fatalf("candidates = %v, want %v", jids, tt.wantJIDs)
			}
			e.assertNothingSent()
			msgs, _, err := e.store.ListMessages(e.ctx(), store.MessageQuery{})
			if err != nil || len(msgs) != 0 {
				t.Fatalf("an unsent message was stored: %+v (err=%v)", msgs, err)
			}
		})
	}
}

func TestSendMessageExactContactNameWithPartialMatchIsAmbiguous(t *testing.T) {
	e := newEnv(t)
	e.fake.SetContacts([]wa.Contact{
		{JID: aliceJID, Name: "Alex"},
		{JID: bobJID, Name: "Alexander"},
	})

	got := callAs[sendResult](e, "send_message", map[string]any{"recipient": "alex", "text": "hi"})

	if got.Status != "ambiguous" || len(got.Candidates) != 2 {
		t.Fatalf("result = %+v, want an ambiguous result listing both contacts", got)
	}
	e.assertNothingSent()
}

func TestSendMessageNeverAutoSendsToPushNameChatOrGroupNames(t *testing.T) {
	tests := []struct {
		name       string
		recipient  string
		wantJID    string
		wantSource string
	}{
		{"exact push name", "Bobby", bobJID, "push_name"},
		{"exact store chat name", "Carol Danvers", carolJID, "chat"},
		{"exact group name", "Family", groupJID, "chat"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			seedAddressBook(e)

			got := callAs[sendResult](e, "send_message", map[string]any{"recipient": tt.recipient, "text": "hi"})

			if got.Status != "ambiguous" || len(got.Candidates) != 1 || got.Candidates[0].JID != tt.wantJID ||
				got.Candidates[0].Source != tt.wantSource {
				t.Fatalf("result = %+v, want one %s candidate %s", got, tt.wantSource, tt.wantJID)
			}
			if !strings.Contains(got.Note, "exact JID") {
				t.Errorf("note = %q, want it to tell the caller to re-run with the exact JID", got.Note)
			}
			e.assertNothingSent()
		})
	}
}

func TestSendMessageCandidatesCarryTheirSource(t *testing.T) {
	e := newEnv(t)
	e.fake.SetContacts([]wa.Contact{
		{JID: aliceJID, Name: "Sam"},
		{JID: bobJID, Name: "Robert", PushName: "sam"},
	})
	e.seedChats(wa.Chat{JID: carolJID, Name: "Sam's group"})

	got := callAs[sendResult](e, "send_message", map[string]any{"recipient": "sam", "text": "hi"})

	sources := map[string]string{}
	for _, c := range got.Candidates {
		sources[c.JID] = c.Source
	}
	want := map[string]string{aliceJID: "contact", bobJID: "push_name", carolJID: "chat"}
	if got.Status != "ambiguous" || len(sources) != len(want) {
		t.Fatalf("result = %+v", got)
	}
	for jid, src := range want {
		if sources[jid] != src {
			t.Errorf("source of %s = %q, want %q", jid, sources[jid], src)
		}
	}
	e.assertNothingSent()
}

func TestSendMessageNameMatchFoldsNonASCIICaseLikeContacts(t *testing.T) {
	tests := []struct {
		name, contact, group, recipient string
	}{
		{"non-ASCII", "\u00e9mile", "\u00c9MILE fans", "\u00e9mile"},
		{"ASCII", "emile", "EMILE fans", "emile"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.fake.SetContacts([]wa.Contact{{JID: aliceJID, Name: tt.contact}})
			e.seedChats(wa.Chat{JID: groupJID, Name: tt.group, IsGroup: true})

			got := callAs[sendResult](e, "send_message", map[string]any{"recipient": tt.recipient, "text": "hi"})

			if got.Status != "ambiguous" || len(got.Candidates) != 2 {
				t.Fatalf("result = %+v, want an ambiguous result listing the contact and the group", got)
			}
			e.assertNothingSent()
		})
	}
}

func TestSendMessageNameFailsClosedWhenChatScanIsTruncated(t *testing.T) {
	e := newEnv(t)
	e.fake.SetContacts([]wa.Contact{{JID: aliceJID, Name: "Zed"}})
	chats := make([]wa.Chat, 0, 2100)
	for i := 0; i < 2100; i++ {
		chats = append(chats, wa.Chat{JID: fmt.Sprintf("1555%07d@s.whatsapp.net", i), Name: fmt.Sprintf("Chat %d", i)})
	}
	e.seedChats(chats...)

	e.fail("send_message", map[string]any{"recipient": "Zed", "text": "hi"}, "too many chats")
	e.assertNothingSent()
}

func TestSendMessageContactNameBlockedByOtherPartialMatches(t *testing.T) {
	tests := []struct {
		name     string
		contacts []wa.Contact
		chats    []wa.Chat
	}{
		{"partial push name", []wa.Contact{{JID: aliceJID, Name: "Mom"}, {JID: bobJID, Name: "Robert", PushName: "Mom's friend"}}, nil},
		{"partial group name", []wa.Contact{{JID: aliceJID, Name: "Mom"}}, []wa.Chat{{JID: groupJID, Name: "Mom and Dad", IsGroup: true}}},
		{"exact chat name of another JID", []wa.Contact{{JID: aliceJID, Name: "Mom"}}, []wa.Chat{{JID: carolJID, Name: "Mom"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.fake.SetContacts(tt.contacts)
			e.seedChats(tt.chats...)

			got := callAs[sendResult](e, "send_message", map[string]any{"recipient": "Mom", "text": "hi"})

			if got.Status != "ambiguous" {
				t.Fatalf("result = %+v, want ambiguous", got)
			}
			e.assertNothingSent()
		})
	}
}

func TestSendMessageRequiresInternationalPrefixForPhoneNumbers(t *testing.T) {
	e := newEnv(t)
	e.fake.SetContacts([]wa.Contact{{JID: aliceJID, Name: "Alice Smith"}})
	e.seedChats(wa.Chat{JID: carolJID, Name: "33633333333"})

	for _, recipient := range []string{"33611111111", "0611111111", "  33 6 11 11 11 11 ", "(33) 6-11.11.11.11"} {
		e.fail("send_message", map[string]any{"recipient": recipient, "text": "hi"}, "country code")
	}
	e.assertNothingSent()

	for _, recipient := range []string{"+33611111111", "  +33 6 11 11 11 11 ", "0033611111111"} {
		got := callAs[sendResult](e, "send_message", map[string]any{"recipient": recipient, "text": "hi"})
		if got.Status != "sent" || got.ChatJID != aliceJID {
			t.Errorf("recipient %q: result = %+v", recipient, got)
		}
	}
}

func TestSendFileRequiresInternationalPrefixForPhoneNumbers(t *testing.T) {
	e := newEnv(t)
	path := newOutboxFile(e, "p.png", []byte("x"))

	e.fail("send_file", map[string]any{"recipient": "33611111111", "path": path}, "country code")
	e.assertNothingSent()
	e.ok("send_file", map[string]any{"recipient": "+33611111111", "path": path})
}

func TestSearchContactsStaysLenientAboutPhoneFormat(t *testing.T) {
	e := newEnv(t)
	seedAddressBook(e)

	res := e.ok("search_contacts", map[string]any{"query": "33611111111"})

	if !strings.Contains(rawJSON(t, res.StructuredContent), aliceJID) {
		t.Fatalf("bare digits must still search: %s", rawJSON(t, res.StructuredContent))
	}
}

func TestSendRejectsRecipientJIDsWithDeviceOrAgentSuffix(t *testing.T) {
	e := newEnv(t, withAllowlist(aliceJID))
	path := newOutboxFile(e, "d.png", []byte("x"))

	for _, jid := range []string{"33611111111:12@s.whatsapp.net", "33611111111.0:3@s.whatsapp.net", "33611111111.1@s.whatsapp.net", "99:1@lid"} {
		e.fail("send_message", map[string]any{"recipient": jid, "text": "hi"}, "plain jid")
		e.fail("send_file", map[string]any{"recipient": jid, "path": path}, "plain jid")
	}
	e.assertNothingSent()
}

func TestSendMessageSameJIDInContactsAndChatsIsNotAmbiguous(t *testing.T) {
	e := newEnv(t)
	e.fake.SetContacts([]wa.Contact{{JID: aliceJID, Name: "Alice"}})
	e.seedChats(wa.Chat{JID: aliceJID, Name: "alice"})

	got := callAs[sendResult](e, "send_message", map[string]any{"recipient": "Alice", "text": "hi"})

	if got.Status != "sent" || got.ChatJID != aliceJID {
		t.Fatalf("result = %+v", got)
	}
}

func TestSendMessageUnknownRecipientIsAnError(t *testing.T) {
	e := newEnv(t)
	seedAddressBook(e)

	e.fail("send_message", map[string]any{"recipient": "Nobody Atall", "text": "hi"}, "no contact or chat matches")
	e.fail("send_message", map[string]any{"recipient": "12", "text": "hi"}, "no contact or chat matches")
	e.assertNothingSent()
}

func TestSendMessageRejectsNonChatJIDs(t *testing.T) {
	e := newEnv(t)
	for _, jid := range []string{"status@broadcast", "123456@newsletter", "120363@broadcast"} {
		e.fail("send_message", map[string]any{"recipient": jid, "text": "hi"}, "user or group jid")
	}
	for _, jid := range []string{"x@evil.example", "@g.us", "nobody@", "a@b@c"} {
		e.fail("send_message", map[string]any{"recipient": jid, "text": "hi"}, "valid whatsapp jid")
	}
	e.assertNothingSent()
}

func TestSendMessageTextLimits(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		wantErr string
	}{
		{"exactly 4096 ascii runes", strings.Repeat("a", 4096), ""},
		{"exactly 4096 multibyte runes", strings.Repeat("é", 4096), ""},
		{"4096 emoji", strings.Repeat("😀", 4096), ""},
		{"4097 ascii runes", strings.Repeat("a", 4097), "at most 4096"},
		{"4097 multibyte runes", strings.Repeat("é", 4097), "at most 4096"},
		{"empty", "", "must not be blank"},
		{"spaces only", "     ", "must not be blank"},
		{"newlines and tabs only", "\n\t\r\n", "must not be blank"},
		{"unicode space only", "  ", "must not be blank"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			args := map[string]any{"recipient": aliceJID, "text": tt.text}
			if tt.wantErr == "" {
				e.ok("send_message", args)
				if sent := e.fake.Sent(); len(sent) != 1 || sent[0].Text != tt.text {
					t.Fatalf("message was altered or not sent")
				}
				return
			}
			e.fail("send_message", args, tt.wantErr)
			e.assertNothingSent()
		})
	}
}

func TestSendMessageAllowlist(t *testing.T) {
	e := newEnv(t, withAllowlist("+33 6 22 22 22 22", groupJID))
	seedAddressBook(e)

	e.fail("send_message", map[string]any{"recipient": aliceJID, "text": "hi"}, "not in the send allowlist")
	e.fail("send_message", map[string]any{"recipient": "Alice Smith", "text": "hi"}, "not in the send allowlist")
	e.fail("send_message", map[string]any{"recipient": "+33611111111", "text": "hi"}, "not in the send allowlist")
	e.fail("send_message", map[string]any{"recipient": "120363999999999999@g.us", "text": "hi"}, "not in the send allowlist")
	e.assertNothingSent()

	for _, recipient := range []string{bobJID, "Bob Builder", "+33622222222", groupJID} {
		e.ok("send_message", map[string]any{"recipient": recipient, "text": "allowed"})
	}
	if n := len(e.fake.Sent()); n != 2 {
		t.Fatalf("sent %d messages, want 2 (recipient aliases dedupe)", n)
	}
}

func TestSendMessageAllowlistRefusalDoesNotConsumeRateSlot(t *testing.T) {
	e := newEnv(t, withAllowlist(bobJID), withRate(time.Hour, 1000))
	seedAddressBook(e)

	e.fail("send_message", map[string]any{"recipient": aliceJID, "text": "hi"}, "allowlist")
	e.ok("send_message", map[string]any{"recipient": bobJID, "text": "hi"})
}

func TestSendMessageAmbiguousResultDoesNotConsumeRateSlot(t *testing.T) {
	e := newEnv(t, withRate(time.Hour, 1000))
	e.fake.SetContacts([]wa.Contact{{JID: aliceJID, Name: "Alex"}, {JID: bobJID, Name: "Alex"}})

	if got := callAs[sendResult](e, "send_message", map[string]any{"recipient": "Alex", "text": "hi"}); got.Status != "ambiguous" {
		t.Fatalf("status = %q", got.Status)
	}
	e.ok("send_message", map[string]any{"recipient": aliceJID, "text": "hi"})
}

func TestSendMessageRateLimitMinimumInterval(t *testing.T) {
	e := newEnv(t, withRate(3*time.Second, 100))

	e.ok("send_message", map[string]any{"recipient": aliceJID, "text": "one"})
	text := e.fail("send_message", map[string]any{"recipient": aliceJID, "text": "two"}, "rate limit")
	if !strings.Contains(text, "retry after 3s") {
		t.Fatalf("error %q does not say when to retry", text)
	}
	if n := len(e.fake.Sent()); n != 1 {
		t.Fatalf("sent %d messages, want 1: the refused send must not reach the client", n)
	}

	e.clock.Advance(1 * time.Second)
	if text := e.fail("send_message", map[string]any{"recipient": aliceJID, "text": "two"}, "rate limit"); !strings.Contains(text, "retry after 2s") {
		t.Fatalf("error %q should report the remaining wait", text)
	}

	e.clock.Advance(2 * time.Second)
	e.ok("send_message", map[string]any{"recipient": aliceJID, "text": "two"})
	if n := len(e.fake.Sent()); n != 2 {
		t.Fatalf("sent %d messages, want 2", n)
	}
}

func TestSendMessageRateLimitIsSharedBetweenRecipients(t *testing.T) {
	e := newEnv(t, withRate(10*time.Second, 100))

	e.ok("send_message", map[string]any{"recipient": aliceJID, "text": "one"})
	e.fail("send_message", map[string]any{"recipient": bobJID, "text": "one"}, "rate limit")
	e.assertNothingSentTo(bobJID)
}

func (e *env) assertNothingSentTo(jid string) {
	e.t.Helper()
	for _, s := range e.fake.Sent() {
		if s.ChatJID == jid {
			e.t.Fatalf("a message was sent to %s: %+v", jid, s)
		}
	}
}

func TestSendMessageHourlyCap(t *testing.T) {
	e := newEnv(t, withRate(time.Second, 2))

	e.ok("send_message", map[string]any{"recipient": aliceJID, "text": "1"})
	e.clock.Advance(2 * time.Second)
	e.ok("send_message", map[string]any{"recipient": aliceJID, "text": "2"})
	e.clock.Advance(2 * time.Second)

	text := e.fail("send_message", map[string]any{"recipient": aliceJID, "text": "3"}, "rate limit")
	if !strings.Contains(text, "retry after 3596s") {
		t.Fatalf("error %q should report when the oldest send leaves the hourly window", text)
	}
	if n := len(e.fake.Sent()); n != 2 {
		t.Fatalf("sent %d, want 2", n)
	}

	e.clock.Advance(time.Hour)
	e.ok("send_message", map[string]any{"recipient": aliceJID, "text": "3"})
	if n := len(e.fake.Sent()); n != 3 {
		t.Fatalf("sent %d, want 3", n)
	}
}

func TestSendMessageClientErrorIsToolErrorAndNothingIsStored(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr string
		notWant string
	}{
		{"generic failure", errors.New("network down"), "unknown_maybe_sent", ""},
		{"phone number scrubbed", errors.New("cannot reach +33612345678 now"), "unknown_maybe_sent", "33612345678"},
		{"not connected is passed through", wa.ErrNotConnected, wa.ErrNotConnected.Error(), ""},
		{"not paired is passed through", wa.ErrNotPaired, wa.ErrNotPaired.Error(), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.fake.SetSendError(tt.err)

			text := e.fail("send_message", map[string]any{"recipient": aliceJID, "text": "hi"}, tt.wantErr)

			if tt.notWant != "" && strings.Contains(text, tt.notWant) {
				t.Fatalf("error %q leaks %q", text, tt.notWant)
			}
			if msgs, _, _ := e.store.ListMessages(e.ctx(), store.MessageQuery{}); len(msgs) != 0 {
				t.Fatalf("failed send was stored: %+v", msgs)
			}
		})
	}
}

func TestSendMessagePolicyGuards(t *testing.T) {
	t.Run("disabled policy refuses even when the tool is registered", func(t *testing.T) {
		e := newEnv(t, withPolicyOff)

		e.fail("send_message", map[string]any{"recipient": aliceJID, "text": "hi"}, "sending is disabled")
		e.assertNothingSent()
	})
	t.Run("missing policy refuses to send", func(t *testing.T) {
		e := newEnv(t, withNoPolicy)

		e.fail("send_message", map[string]any{"recipient": aliceJID, "text": "hi"}, "send policy is not configured")
		e.assertNothingSent()
	})
}

func newOutboxFile(e *env, name string, data []byte) string {
	return e.writeFile(filepath.Join(e.outbox, name), data)
}

func TestSendFileFromOutboxAndDownloadDir(t *testing.T) {
	tests := []struct {
		name         string
		dir          func(e *env) string
		fileName     string
		wantMime     string
		wantMedia    wa.MediaType
		caption      string
		wantFileName string
	}{
		{"png in outbox", func(e *env) string { return e.outbox }, "photo.png", "image/png", wa.MediaImage, "look at this", "photo.png"},
		{"jpg uppercase extension", func(e *env) string { return e.outbox }, "PHOTO.JPG", "image/jpeg", wa.MediaImage, "", "PHOTO.JPG"},
		{"pdf in outbox", func(e *env) string { return e.outbox }, "report.pdf", "application/pdf", wa.MediaDocument, "Q3", "report.pdf"},
		{"html strips charset parameter", func(e *env) string { return e.outbox }, "page.html", "text/html", wa.MediaDocument, "", "page.html"},
		{"unknown extension", func(e *env) string { return e.outbox }, "data.zzzqq", "application/octet-stream", wa.MediaDocument, "", "data.zzzqq"},
		{"no extension", func(e *env) string { return e.outbox }, "README", "application/octet-stream", wa.MediaDocument, "", "README"},
		{"file in download dir", func(e *env) string { return e.downloads }, "received.png", "image/png", wa.MediaImage, "", "received.png"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			seedAddressBook(e)
			data := []byte("payload-" + tt.fileName)
			path := e.writeFile(filepath.Join(tt.dir(e), tt.fileName), data)

			got := callAs[sendResult](e, "send_file", map[string]any{"recipient": "Alice Smith", "path": path, "caption": tt.caption})

			if got.Status != "sent" || got.ChatJID != aliceJID || got.MessageID == "" {
				t.Fatalf("result = %+v", got)
			}
			sent := e.fake.Sent()
			if len(sent) != 1 || sent[0].File == nil || sent[0].ChatJID != aliceJID {
				t.Fatalf("sent = %+v", sent)
			}
			f := sent[0].File
			if string(f.Data) != string(data) || f.FileName != tt.wantFileName || f.MimeType != tt.wantMime || f.Caption != tt.caption {
				t.Fatalf("outgoing file = {data:%q name:%q mime:%q caption:%q}", f.Data, f.FileName, f.MimeType, f.Caption)
			}
			saved, err := e.store.GetMessage(e.ctx(), aliceJID, got.MessageID)
			if err != nil {
				t.Fatalf("sent file not stored: %v", err)
			}
			if !saved.FromMe || saved.Text != tt.caption || saved.Media == nil || saved.Media.Type != tt.wantMedia ||
				saved.Media.MimeType != tt.wantMime || saved.Media.FileName != tt.wantFileName || saved.Media.FileLength != uint64(len(data)) {
				t.Fatalf("stored = %+v media=%+v", saved, saved.Media)
			}
		})
	}
}

func TestSendFileAcceptsRelativeAndNestedOutboxPaths(t *testing.T) {
	e := newEnv(t)
	nested := e.writeFile(filepath.Join(e.outbox, "sub", "dir", "n.png"), []byte("x"))

	e.ok("send_file", map[string]any{"recipient": aliceJID, "path": nested})

	if sent := e.fake.Sent(); len(sent) != 1 || sent[0].File.FileName != "n.png" {
		t.Fatalf("sent = %+v", sent)
	}
}

func TestSendFileRefusesPathsOutsideAllowedDirectories(t *testing.T) {
	e := newEnv(t)
	secret := e.writeFile(filepath.Join(e.root, "secret.txt"), []byte("TOP SECRET"))
	inside := newOutboxFile(e, "ok.txt", []byte("fine"))
	linkToOutside := filepath.Join(e.outbox, "innocent.txt")
	if err := os.Symlink(secret, linkToOutside); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	dirLink := filepath.Join(e.outbox, "dirlink")
	if err := os.Symlink(e.root, dirLink); err != nil {
		t.Fatal(err)
	}
	e.writeFile(filepath.Join(e.root, "outbox-evil", "x.txt"), []byte("sibling with shared prefix"))

	tests := []struct {
		name string
		path string
	}{
		{"absolute system file", "/etc/passwd"},
		{"file next to the outbox", secret},
		{"dot dot traversal out of the outbox", filepath.Join(e.outbox, "..", "secret.txt")},
		{"dot dot traversal that comes back", filepath.Join(e.outbox, "sub", "..", "..", "secret.txt")},
		{"symlink inside outbox pointing outside", linkToOutside},
		{"file reached through a directory symlink", filepath.Join(dirLink, "secret.txt")},
		{"sibling directory sharing the outbox prefix", filepath.Join(e.root, "outbox-evil", "x.txt")},
		{"database file", filepath.Join(e.root, "messages.db")},
		{"directory", e.outbox},
		{"directory in the download dir", e.downloads},
		{"missing file", filepath.Join(e.outbox, "missing.png")},
		{"missing download dir file", filepath.Join(e.downloads, "missing.png")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.fail("send_file", map[string]any{"recipient": aliceJID, "path": tt.path}, "inside the outbox or download directory")
			e.assertNothingSent()
		})
	}
	e.ok("send_file", map[string]any{"recipient": aliceJID, "path": inside})
}

func TestSendFileRejectsEmptyFile(t *testing.T) {
	e := newEnv(t)
	empty := newOutboxFile(e, "empty.txt", nil)

	e.fail("send_file", map[string]any{"recipient": aliceJID, "path": empty}, "file is empty")
	e.assertNothingSent()
}

func TestSendFileRejectsFilesOverTheSizeLimit(t *testing.T) {
	const maxBytes = 64 << 20
	e := newEnv(t)
	big := filepath.Join(e.outbox, "big.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxBytes + 1); err != nil {
		_ = f.Close()
		t.Skipf("cannot create a sparse file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	e.fail("send_file", map[string]any{"recipient": aliceJID, "path": big}, "larger than the 64 MiB limit")
	e.assertNothingSent()
}

func TestSendFileCaptionLimit(t *testing.T) {
	e := newEnv(t)
	path := newOutboxFile(e, "c.png", []byte("x"))

	e.fail("send_file", map[string]any{"recipient": aliceJID, "path": path, "caption": strings.Repeat("é", 4097)}, "caption must be at most")
	e.assertNothingSent()
	e.ok("send_file", map[string]any{"recipient": aliceJID, "path": path, "caption": strings.Repeat("é", 4096)})
}

func TestSendFileRecipientHandling(t *testing.T) {
	e := newEnv(t, withAllowlist(bobJID))
	e.fake.SetContacts(append([]wa.Contact{{JID: carolJID, Name: "Alice Smith"}}, aliceAndBob()...))
	path := newOutboxFile(e, "doc.pdf", []byte("pdf"))

	t.Run("ambiguous name sends nothing", func(t *testing.T) {
		got := callAs[sendResult](e, "send_file", map[string]any{"recipient": "Alice Smith", "path": path})
		if got.Status != "ambiguous" || len(got.Candidates) != 2 {
			t.Fatalf("result = %+v", got)
		}
		e.assertNothingSent()
	})
	t.Run("allowlist refusal sends nothing", func(t *testing.T) {
		e.fail("send_file", map[string]any{"recipient": aliceJID, "path": path}, "not in the send allowlist")
		e.assertNothingSent()
	})
	t.Run("broadcast jid refused", func(t *testing.T) {
		e.fail("send_file", map[string]any{"recipient": "status@broadcast", "path": path}, "user or group jid")
		e.assertNothingSent()
	})
	t.Run("unknown name refused", func(t *testing.T) {
		e.fail("send_file", map[string]any{"recipient": "Nobody Atall", "path": path}, "no contact or chat matches")
		e.assertNothingSent()
	})
	t.Run("allowed recipient", func(t *testing.T) {
		e.ok("send_file", map[string]any{"recipient": "Bob Builder", "path": path})
		if sent := e.fake.Sent(); len(sent) != 1 || sent[0].ChatJID != bobJID {
			t.Fatalf("sent = %+v", sent)
		}
	})
}

func aliceAndBob() []wa.Contact {
	return []wa.Contact{
		{JID: aliceJID, Name: "Alice Smith", PushName: "Ali"},
		{JID: bobJID, Name: "Bob Builder", PushName: "Bobby"},
	}
}

func TestSendFileSharesRateLimitWithSendMessage(t *testing.T) {
	e := newEnv(t, withRate(time.Minute, 100))
	path := newOutboxFile(e, "r.png", []byte("x"))

	e.ok("send_message", map[string]any{"recipient": aliceJID, "text": "first"})
	e.fail("send_file", map[string]any{"recipient": aliceJID, "path": path}, "rate limit")

	if n := len(e.fake.Sent()); n != 1 {
		t.Fatalf("sent %d, want 1", n)
	}
}

func TestSendFileClientErrorIsToolError(t *testing.T) {
	e := newEnv(t)
	e.fake.SetSendError(fmt.Errorf("%w: upload failed", wa.ErrNotSent))
	path := newOutboxFile(e, "u.png", []byte("x"))

	e.fail("send_file", map[string]any{"recipient": aliceJID, "path": path}, "upload failed")
	if msgs, _, _ := e.store.ListMessages(e.ctx(), store.MessageQuery{}); len(msgs) != 0 {
		t.Fatalf("failed send was stored: %+v", msgs)
	}
}

func TestSendFileClassifiesVideoAndAudioByMimeType(t *testing.T) {
	for ext, mt := range map[string]string{".xtvid": "video/x-test", ".xtaud": "audio/x-test"} {
		if err := mime.AddExtensionType(ext, mt); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		file string
		want wa.MediaType
	}{
		{"clip.xtvid", wa.MediaVideo},
		{"voice.xtaud", wa.MediaAudio},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			e := newEnv(t)
			path := newOutboxFile(e, tt.file, []byte("media"))

			got := callAs[sendResult](e, "send_file", map[string]any{"recipient": aliceJID, "path": path})

			saved, err := e.store.GetMessage(e.ctx(), aliceJID, got.MessageID)
			if err != nil || saved.Media == nil || saved.Media.Type != tt.want {
				t.Fatalf("stored media = %+v (err=%v), want type %s", saved.Media, err, tt.want)
			}
		})
	}
}

func TestSendMessageAllowlistIsKeyedByServerAndUser(t *testing.T) {
	e := newEnv(t, withAllowlist("+33 6 11 11 11 11"))

	e.fail("send_message", map[string]any{"recipient": "33611111111@lid", "text": "hi"}, "not in the send allowlist")
	e.assertNothingSent()
	e.ok("send_message", map[string]any{"recipient": aliceJID, "text": "hi"})
}

func TestFailedSendRefundsTheRateSlot(t *testing.T) {
	t.Run("send_message", func(t *testing.T) {
		e := newEnv(t, withRate(time.Hour, 1))
		e.fake.SetSendError(fmt.Errorf("%w: network down", wa.ErrNotSent))
		e.fail("send_message", map[string]any{"recipient": aliceJID, "text": "hi"}, "send failed")

		e.fake.SetSendError(nil)
		e.ok("send_message", map[string]any{"recipient": aliceJID, "text": "hi"})
		e.fail("send_message", map[string]any{"recipient": aliceJID, "text": "again"}, "rate limit")
	})
	t.Run("send_file", func(t *testing.T) {
		e := newEnv(t, withRate(time.Hour, 1))
		path := newOutboxFile(e, "r.png", []byte("x"))
		e.fake.SetSendError(fmt.Errorf("%w: upload failed", wa.ErrNotSent))
		e.fail("send_file", map[string]any{"recipient": aliceJID, "path": path}, "send failed")

		e.fake.SetSendError(nil)
		e.ok("send_file", map[string]any{"recipient": aliceJID, "path": path})
	})
}

func TestSendErrorsThatMayHaveBeenDeliveredKeepTheRateSlot(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		refund bool
	}{
		{"ack timeout", errors.New("timed out waiting for message send response"), false},
		{"info query timeout", errors.New("info query timed out"), false},
		{"context deadline", context.DeadlineExceeded, false},
		{"unclassified failure", errors.New("network down"), false},
		{"pre-send failure", fmt.Errorf("%w: upload failed", wa.ErrNotSent), true},
		{"not connected", wa.ErrNotConnected, true},
		{"not paired", wa.ErrNotPaired, true},
	}
	for _, tt := range tests {
		for _, tool := range []string{"send_message", "send_file"} {
			t.Run(tool+"/"+tt.name, func(t *testing.T) {
				e := newEnv(t, withRate(time.Hour, 1))
				args := map[string]any{"recipient": aliceJID}
				if tool == "send_message" {
					args["text"] = "hi"
				} else {
					args["path"] = newOutboxFile(e, "r.png", []byte("x"))
				}
				e.fake.SetSendError(tt.err)
				e.fail(tool, args, "")

				e.fake.SetSendError(nil)
				args["idempotency_key"] = "fresh-attempt-to-check-rate-slot"
				res := e.call(tool, args)
				if refunded := !res.IsError; refunded != tt.refund {
					t.Fatalf("second send succeeded = %v, want %v (slot refunded)", refunded, tt.refund)
				}
			})
		}
	}
}

func TestSendChecksConnectionBeforeConsumingARateSlot(t *testing.T) {
	tests := []struct {
		name    string
		status  wa.Status
		wantErr error
	}{
		{"not paired", wa.Status{State: wa.StateNeedsQR}, wa.ErrNotPaired},
		{"not connected", wa.Status{State: wa.StateDisconnected, Paired: true, AccountJID: selfJID}, wa.ErrNotConnected},
		{"connecting", wa.Status{State: wa.StateConnecting, Paired: true, AccountJID: selfJID}, wa.ErrNotConnected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, withRate(time.Hour, 1), withStatus(tt.status))
			path := newOutboxFile(e, "c.png", []byte("x"))

			e.fail("send_message", map[string]any{"recipient": aliceJID, "text": "hi"}, tt.wantErr.Error())
			e.fail("send_file", map[string]any{"recipient": aliceJID, "path": path}, tt.wantErr.Error())
			e.assertNothingSent()

			e.fake.SetStatus(wa.Status{State: wa.StateConnected, Paired: true, AccountJID: selfJID})
			e.ok("send_message", map[string]any{"recipient": aliceJID, "text": "slot was not consumed"})
		})
	}
}

func TestSendFileRejectsAudioCaptions(t *testing.T) {
	if err := mime.AddExtensionType(".xtaud", "audio/x-test"); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, withRate(time.Hour, 1))
	path := newOutboxFile(e, "voice.xtaud", []byte("audio"))

	e.fail("send_file", map[string]any{"recipient": aliceJID, "path": path, "caption": "listen"}, "captions are not supported for audio")
	e.assertNothingSent()
	e.ok("send_file", map[string]any{"recipient": aliceJID, "path": path})
}

func TestSendFileRecordsOutgoingMediaTypeLikeTheClient(t *testing.T) {
	tests := map[string]wa.MediaType{"anim.gif": wa.MediaDocument, "pic.webp": wa.MediaImage, "pic.png": wa.MediaImage}
	for file, want := range tests {
		t.Run(file, func(t *testing.T) {
			e := newEnv(t)
			path := newOutboxFile(e, file, []byte("x"))

			got := callAs[sendResult](e, "send_file", map[string]any{"recipient": aliceJID, "path": path})

			saved, err := e.store.GetMessage(e.ctx(), aliceJID, got.MessageID)
			if err != nil || saved.Media == nil || saved.Media.Type != want {
				t.Fatalf("stored media = %+v (err=%v), want type %s", saved.Media, err, want)
			}
		})
	}
}

func TestSendFileNeverSendsFilesFromTheDataDir(t *testing.T) {
	e := newEnv(t, func(o *options) { o.downloadIsDataDir = true })
	token := e.writeFile(filepath.Join(e.root, "http-token"), []byte("secret-token"))
	nested := e.writeFile(filepath.Join(e.root, "downloads", "fine.png"), []byte("x"))

	for _, path := range []string{filepath.Join(e.root, "messages.db"), token, nested} {
		e.fail("send_file", map[string]any{"recipient": aliceJID, "path": path}, "inside the outbox or download directory")
	}
	e.assertNothingSent()
	e.ok("send_file", map[string]any{"recipient": aliceJID, "path": newOutboxFile(e, "ok.png", []byte("x"))})
}

func TestSendFileRefusesHardLinks(t *testing.T) {
	e := newEnv(t)
	target := e.writeFile(filepath.Join(e.root, "secret.txt"), []byte("TOP SECRET"))
	link := filepath.Join(e.outbox, "innocent.txt")
	if err := os.Link(target, link); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("link counts are not checked on this OS")
	}

	e.fail("send_file", map[string]any{"recipient": aliceJID, "path": link}, "inside the outbox or download directory")
	e.assertNothingSent()
}
