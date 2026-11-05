package tools_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

type chatItem struct {
	JID               string `json:"jid"`
	Name              string `json:"name"`
	IsGroup           bool   `json:"is_group"`
	LastMessageTime   string `json:"last_message_time"`
	LastMessageText   string `json:"last_message_text"`
	LastMessageFromMe bool   `json:"last_message_from_me"`
	LastSenderJID     string `json:"last_sender_jid"`
}

type chatsOut struct {
	Chats      []chatItem `json:"chats"`
	HasMore    bool       `json:"has_more"`
	NextOffset int        `json:"next_offset"`
}

type msgItem struct {
	ID         string `json:"id"`
	ChatJID    string `json:"chat_jid"`
	SenderJID  string `json:"sender_jid"`
	SenderName string `json:"sender_name"`
	Timestamp  string `json:"timestamp"`
	FromMe     bool   `json:"from_me"`
	Text       string `json:"text"`
	MediaType  string `json:"media_type"`
	FileName   string `json:"file_name"`
}

type msgsOut struct {
	Messages   []msgItem `json:"messages"`
	HasMore    bool      `json:"has_more"`
	NextOffset int       `json:"next_offset"`
}

type contextOut struct {
	Before  []msgItem `json:"before"`
	Message msgItem   `json:"message"`
	After   []msgItem `json:"after"`
}

type contactsOut struct {
	Contacts []struct {
		JID      string `json:"jid"`
		Name     string `json:"name"`
		PushName string `json:"push_name"`
		Phone    string `json:"phone"`
	} `json:"contacts"`
}

func chatJIDs(chats []chatItem) []string {
	out := make([]string, len(chats))
	for i, c := range chats {
		out[i] = c.JID
	}
	return out
}

func msgIDs(msgs []msgItem) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.ID
	}
	return out
}

func assertIDs(t *testing.T, what string, got []msgItem, want ...string) {
	t.Helper()
	if ids := msgIDs(got); !slices.Equal(ids, want) {
		t.Fatalf("%s ids = %v, want %v", what, ids, want)
	}
}

func seedFiveChats(e *env) {
	e.seedChats(
		wa.Chat{JID: aliceJID, Name: "Alice"},
		wa.Chat{JID: bobJID, Name: "bob"},
		wa.Chat{JID: carolJID, Name: "Carol"},
		wa.Chat{JID: groupJID, Name: "Family", IsGroup: true},
		wa.Chat{JID: "33644444444@s.whatsapp.net", Name: "Dave"},
	)
	e.seedMessages(
		msg("a1", aliceJID, epoch.Add(1*time.Hour), "alice latest"),
		msg("b1", bobJID, epoch.Add(3*time.Hour), "bob latest"),
		msg("c1", carolJID, epoch.Add(2*time.Hour), "carol latest"),
		msg("g1", groupJID, epoch.Add(5*time.Hour), "group latest"),
		msg("d1", "33644444444@s.whatsapp.net", epoch.Add(4*time.Hour), "dave latest"),
	)
}

func TestListChatsOrdersByLastActivityNewestFirst(t *testing.T) {
	e := newEnv(t)
	seedFiveChats(e)

	got := callAs[chatsOut](e, "list_chats", map[string]any{})

	want := []string{groupJID, "33644444444@s.whatsapp.net", bobJID, carolJID, aliceJID}
	if !slices.Equal(chatJIDs(got.Chats), want) {
		t.Fatalf("order = %v, want %v", chatJIDs(got.Chats), want)
	}
	if got.HasMore || got.NextOffset != 0 {
		t.Fatalf("has_more=%v next_offset=%d, want false/0", got.HasMore, got.NextOffset)
	}
	first := got.Chats[0]
	if !first.IsGroup || first.Name != "Family" || first.LastMessageText != "group latest" ||
		first.LastMessageTime != epoch.Add(5*time.Hour).Format(time.RFC3339) {
		t.Fatalf("first chat = %+v", first)
	}
}

func TestListChatsSortByNameIsCaseInsensitive(t *testing.T) {
	e := newEnv(t)
	seedFiveChats(e)

	got := callAs[chatsOut](e, "list_chats", map[string]any{"sort_by": "name"})

	names := make([]string, len(got.Chats))
	for i, c := range got.Chats {
		names[i] = c.Name
	}
	if want := []string{"Alice", "bob", "Carol", "Dave", "Family"}; !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestListChatsPaginationWalksEveryChatExactlyOnce(t *testing.T) {
	e := newEnv(t)
	seedFiveChats(e)

	tests := []struct {
		offset, limit  int
		wantN          int
		wantHasMore    bool
		wantNextOffset int
	}{
		{0, 2, 2, true, 2},
		{2, 2, 2, true, 4},
		{4, 2, 1, false, 0},
		{0, 5, 5, false, 0},
		{3, 5, 2, false, 0},
		{10, 5, 0, false, 0},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("offset %d limit %d", tt.offset, tt.limit), func(t *testing.T) {
			got := callAs[chatsOut](e, "list_chats", map[string]any{"limit": tt.limit, "offset": tt.offset})
			if len(got.Chats) != tt.wantN || got.HasMore != tt.wantHasMore || got.NextOffset != tt.wantNextOffset {
				t.Fatalf("got n=%d has_more=%v next_offset=%d, want n=%d has_more=%v next_offset=%d",
					len(got.Chats), got.HasMore, got.NextOffset, tt.wantN, tt.wantHasMore, tt.wantNextOffset)
			}
		})
	}

	var all []string
	offset := 0
	for range 10 {
		page := callAs[chatsOut](e, "list_chats", map[string]any{"limit": 2, "offset": offset})
		all = append(all, chatJIDs(page.Chats)...)
		if !page.HasMore {
			break
		}
		offset = page.NextOffset
	}
	if len(all) != 5 || len(slices.Compact(slices.Sorted(slices.Values(all)))) != 5 {
		t.Fatalf("following next_offset visited %v, want 5 distinct chats", all)
	}
}

func TestListChatsQueryFilter(t *testing.T) {
	e := newEnv(t)
	seedFiveChats(e)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"name substring ignores case", "ALI", []string{aliceJID}},
		{"lowercase name matches capitalised chat", "carol", []string{carolJID}},
		{"jid fragment", "3622222", []string{bobJID}},
		{"surrounding whitespace trimmed", "  dave  ", []string{"33644444444@s.whatsapp.net"}},
		{"no match", "zzz-nothing", nil},
		{"percent is literal", "%", nil},
		{"underscore is literal", "_", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := callAs[chatsOut](e, "list_chats", map[string]any{"query": tt.query})
			if !slices.Equal(chatJIDs(got.Chats), tt.want) && !(len(got.Chats) == 0 && len(tt.want) == 0) {
				t.Fatalf("query %q -> %v, want %v", tt.query, chatJIDs(got.Chats), tt.want)
			}
		})
	}
}

func TestListChatsEmptyResultIsEmptyArrayNotNull(t *testing.T) {
	e := newEnv(t)

	raw := rawJSON(t, e.ok("list_chats", map[string]any{}).StructuredContent)

	if !strings.Contains(raw, `"chats":[]`) {
		t.Fatalf("empty result = %s, want an empty chats array", raw)
	}
}

func TestListChatsReportsLastMessageFromMeAndSender(t *testing.T) {
	e := newEnv(t)
	own := wa.Message{ID: "o1", ChatJID: aliceJID, SenderJID: selfJID, Timestamp: epoch, FromMe: true, Text: "mine"}
	e.seedMessages(own)

	got := callAs[chatsOut](e, "list_chats", map[string]any{})

	if len(got.Chats) != 1 || !got.Chats[0].LastMessageFromMe || got.Chats[0].LastSenderJID != selfJID {
		t.Fatalf("chats = %+v", got.Chats)
	}
}

func seedConversation(e *env) {
	base := epoch
	e.seedMessages(
		wa.Message{ID: "m1", ChatJID: aliceJID, SenderJID: aliceJID, SenderName: "Alice", Timestamp: base.Add(1 * time.Minute), Text: "Meeting at noon"},
		wa.Message{ID: "m2", ChatJID: aliceJID, SenderJID: selfJID, Timestamp: base.Add(2 * time.Minute), FromMe: true, Text: "ok, see you"},
		wa.Message{ID: "m3", ChatJID: aliceJID, SenderJID: aliceJID, SenderName: "Alice", Timestamp: base.Add(3 * time.Minute), Text: "reschedule the MEETING please"},
		wa.Message{ID: "m4", ChatJID: aliceJID, SenderJID: aliceJID, SenderName: "Alice", Timestamp: base.Add(4 * time.Minute), Text: "100% sure", Media: &wa.Media{Type: wa.MediaDocument, FileName: "plan.pdf", MimeType: "application/pdf"}},
		wa.Message{ID: "m5", ChatJID: aliceJID, SenderJID: aliceJID, SenderName: "Alice", Timestamp: base.Add(5 * time.Minute), Text: "under_score"},
		wa.Message{ID: "x1", ChatJID: bobJID, SenderJID: bobJID, SenderName: "Bob", Timestamp: base.Add(90 * time.Second), Text: "other chat meeting"},
	)
}

func TestListMessagesDefaultsNewestFirstAndMapsFields(t *testing.T) {
	e := newEnv(t)
	seedConversation(e)

	got := callAs[msgsOut](e, "list_messages", map[string]any{"chat_jid": aliceJID})

	assertIDs(t, "messages", got.Messages, "m5", "m4", "m3", "m2", "m1")
	byID := map[string]msgItem{}
	for _, m := range got.Messages {
		byID[m.ID] = m
	}
	m1 := byID["m1"]
	if m1.ChatJID != aliceJID || m1.SenderJID != aliceJID || m1.SenderName != "Alice" || m1.Text != "Meeting at noon" ||
		m1.FromMe || m1.Timestamp != epoch.Add(time.Minute).Format(time.RFC3339) || m1.MediaType != "" || m1.FileName != "" {
		t.Fatalf("m1 = %+v", m1)
	}
	if m2 := byID["m2"]; !m2.FromMe || m2.SenderJID != selfJID {
		t.Fatalf("m2 = %+v", m2)
	}
	if m4 := byID["m4"]; m4.MediaType != "document" || m4.FileName != "plan.pdf" {
		t.Fatalf("m4 media fields = %+v", m4)
	}
}

func TestListMessagesOldestFirst(t *testing.T) {
	e := newEnv(t)
	seedConversation(e)

	got := callAs[msgsOut](e, "list_messages", map[string]any{"chat_jid": aliceJID, "oldest_first": true})

	assertIDs(t, "messages", got.Messages, "m1", "m2", "m3", "m4", "m5")
}

func TestListMessagesWithoutChatSpansAllChats(t *testing.T) {
	e := newEnv(t)
	seedConversation(e)

	got := callAs[msgsOut](e, "list_messages", map[string]any{"oldest_first": true})

	assertIDs(t, "messages", got.Messages, "m1", "x1", "m2", "m3", "m4", "m5")
}

func TestListMessagesFilters(t *testing.T) {
	e := newEnv(t)
	seedConversation(e)
	at := func(min int) string { return epoch.Add(time.Duration(min) * time.Minute).Format(time.RFC3339) }

	tests := []struct {
		name string
		args map[string]any
		want []string
	}{
		{"chat", map[string]any{"chat_jid": bobJID}, []string{"x1"}},
		{"sender", map[string]any{"sender_jid": selfJID}, []string{"m2"}},
		{"sender and chat", map[string]any{"sender_jid": bobJID, "chat_jid": aliceJID}, nil},
		{"text is case insensitive", map[string]any{"query": "meeting", "chat_jid": aliceJID}, []string{"m3", "m1"}},
		{"text across chats", map[string]any{"query": "MEETING"}, []string{"m3", "x1", "m1"}},
		{"percent is literal", map[string]any{"query": "%"}, []string{"m4"}},
		{"underscore is literal", map[string]any{"query": "_"}, []string{"m5"}},
		{"after is inclusive", map[string]any{"after": at(3), "chat_jid": aliceJID}, []string{"m5", "m4", "m3"}},
		{"before is exclusive", map[string]any{"before": at(3), "chat_jid": aliceJID}, []string{"m2", "m1"}},
		{"after and before window", map[string]any{"after": at(2), "before": at(4), "chat_jid": aliceJID}, []string{"m3", "m2"}},
		{"offset timestamps are normalised", map[string]any{"after": epoch.Add(3 * time.Minute).In(time.FixedZone("x", 2*3600)).Format(time.RFC3339), "chat_jid": aliceJID}, []string{"m5", "m4", "m3"}},
		{"nothing matches", map[string]any{"query": "no-such-text"}, nil},
		{"before beyond year 2261 is clamped", map[string]any{"before": "9999-12-31T00:00:00Z", "chat_jid": aliceJID}, []string{"m5", "m4", "m3", "m2", "m1"}},
		{"after before year 1678 is clamped", map[string]any{"after": "1000-01-01T00:00:00Z", "chat_jid": aliceJID}, []string{"m5", "m4", "m3", "m2", "m1"}},
		{"after beyond year 2261 matches nothing", map[string]any{"after": "9999-12-31T00:00:00Z", "chat_jid": aliceJID}, nil},
		{"before before year 1678 matches nothing", map[string]any{"before": "1000-01-01T00:00:00Z", "chat_jid": aliceJID}, nil},
		{"far window", map[string]any{"after": "1000-01-01T00:00:00Z", "before": "9999-12-31T00:00:00Z", "chat_jid": aliceJID}, []string{"m5", "m4", "m3", "m2", "m1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := callAs[msgsOut](e, "list_messages", tt.args)
			if ids := msgIDs(got.Messages); !slices.Equal(ids, tt.want) && !(len(ids) == 0 && len(tt.want) == 0) {
				t.Fatalf("ids = %v, want %v", ids, tt.want)
			}
		})
	}
}

func TestListMessagesPagination(t *testing.T) {
	e := newEnv(t)
	seedConversation(e)
	base := map[string]any{"chat_jid": aliceJID, "oldest_first": true}
	page := func(limit, offset int) msgsOut {
		args := map[string]any{"limit": limit, "offset": offset}
		for k, v := range base {
			args[k] = v
		}
		return callAs[msgsOut](e, "list_messages", args)
	}

	p1 := page(2, 0)
	assertIDs(t, "page 1", p1.Messages, "m1", "m2")
	if !p1.HasMore || p1.NextOffset != 2 {
		t.Fatalf("page 1 has_more=%v next_offset=%d", p1.HasMore, p1.NextOffset)
	}
	p2 := page(2, p1.NextOffset)
	assertIDs(t, "page 2", p2.Messages, "m3", "m4")
	if !p2.HasMore || p2.NextOffset != 4 {
		t.Fatalf("page 2 has_more=%v next_offset=%d", p2.HasMore, p2.NextOffset)
	}
	p3 := page(2, p2.NextOffset)
	assertIDs(t, "page 3", p3.Messages, "m5")
	if p3.HasMore || p3.NextOffset != 0 {
		t.Fatalf("last page has_more=%v next_offset=%d, want false/0", p3.HasMore, p3.NextOffset)
	}
	exact := page(5, 0)
	if exact.HasMore || len(exact.Messages) != 5 {
		t.Fatalf("a page that ends exactly at the last row must not claim more: %+v", exact)
	}
}

func TestListMessagesDefaultLimitIsTwenty(t *testing.T) {
	e := newEnv(t)
	for i := range 25 {
		e.seedMessages(msg(fmt.Sprintf("n%02d", i), aliceJID, epoch.Add(time.Duration(i)*time.Minute), "x"))
	}

	got := callAs[msgsOut](e, "list_messages", map[string]any{})

	if len(got.Messages) != 20 || !got.HasMore || got.NextOffset != 20 {
		t.Fatalf("n=%d has_more=%v next_offset=%d, want 20/true/20", len(got.Messages), got.HasMore, got.NextOffset)
	}
}

func TestGetMessageContextWindows(t *testing.T) {
	e := newEnv(t)
	for i := 1; i <= 7; i++ {
		e.seedMessages(msg(fmt.Sprintf("c%d", i), aliceJID, epoch.Add(time.Duration(i)*time.Minute), fmt.Sprintf("body %d", i)))
	}
	e.seedMessages(msg("other", bobJID, epoch.Add(4*time.Minute+time.Second), "must never appear"))
	ptr := func(n int) *int { return &n }

	tests := []struct {
		name       string
		before     *int
		after      *int
		wantBefore []string
		wantAfter  []string
	}{
		{"defaults to five each side", nil, nil, []string{"c1", "c2", "c3"}, []string{"c5", "c6", "c7"}},
		{"explicit windows", ptr(2), ptr(1), []string{"c2", "c3"}, []string{"c5"}},
		{"zero windows", ptr(0), ptr(0), nil, nil},
		{"only before", ptr(1), ptr(0), []string{"c3"}, nil},
		{"windows larger than the data", ptr(50), ptr(50), []string{"c1", "c2", "c3"}, []string{"c5", "c6", "c7"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := map[string]any{"message_id": "c4", "chat_jid": aliceJID}
			if tt.before != nil {
				args["before"] = *tt.before
			}
			if tt.after != nil {
				args["after"] = *tt.after
			}

			got := callAs[contextOut](e, "get_message_context", args)

			if got.Message.ID != "c4" || got.Message.Text != "body 4" {
				t.Fatalf("target = %+v", got.Message)
			}
			if ids := msgIDs(got.Before); !slices.Equal(ids, tt.wantBefore) && !(len(ids) == 0 && len(tt.wantBefore) == 0) {
				t.Fatalf("before = %v, want %v", ids, tt.wantBefore)
			}
			if ids := msgIDs(got.After); !slices.Equal(ids, tt.wantAfter) && !(len(ids) == 0 && len(tt.wantAfter) == 0) {
				t.Fatalf("after = %v, want %v", ids, tt.wantAfter)
			}
		})
	}
}

func TestGetMessageContextEmptyWindowsAreArraysNotNull(t *testing.T) {
	e := newEnv(t)
	e.seedMessages(msg("only", aliceJID, epoch, "solo"))

	raw := rawJSON(t, e.ok("get_message_context", map[string]any{"message_id": "only"}).StructuredContent)

	if !strings.Contains(raw, `"before":[]`) || !strings.Contains(raw, `"after":[]`) {
		t.Fatalf("windows must be empty arrays: %s", raw)
	}
}

func TestGetMessageContextTrimsMessageID(t *testing.T) {
	e := newEnv(t)
	e.seedMessages(msg("pad", aliceJID, epoch, "x"))

	got := callAs[contextOut](e, "get_message_context", map[string]any{"message_id": "  pad  "})

	if got.Message.ID != "pad" {
		t.Fatalf("target = %+v", got.Message)
	}
}

func TestGetMessageContextNotFound(t *testing.T) {
	e := newEnv(t)
	e.seedMessages(msg("present", aliceJID, epoch, "x"))

	e.fail("get_message_context", map[string]any{"message_id": "absent"}, "message not found")
	e.fail("get_message_context", map[string]any{"message_id": "present", "chat_jid": bobJID}, "message not found")
}

func TestGetMessageContextAmbiguousIDListsChats(t *testing.T) {
	e := newEnv(t)
	e.seedMessages(
		msg("DUP", aliceJID, epoch, "in alice"),
		msg("DUP", bobJID, epoch.Add(time.Minute), "in bob"),
		msg("DUP", groupJID, epoch.Add(2*time.Minute), "in group"),
	)

	text := e.fail("get_message_context", map[string]any{"message_id": "DUP"}, "pass chat_jid")

	for _, jid := range []string{aliceJID, bobJID, groupJID} {
		if !strings.Contains(text, jid) {
			t.Errorf("error %q does not list chat %s", text, jid)
		}
	}
	got := callAs[contextOut](e, "get_message_context", map[string]any{"message_id": "DUP", "chat_jid": bobJID})
	if got.Message.Text != "in bob" {
		t.Fatalf("disambiguated target = %+v", got.Message)
	}
}

func TestSearchContactsMergesClientContactsWithStoreDMChats(t *testing.T) {
	e := newEnv(t)
	e.fake.SetContacts([]wa.Contact{
		{JID: aliceJID, Name: "Alice Smith", PushName: "Ali"},
		{JID: bobJID, Name: "Bob Builder", PushName: "Bobby"},
		{JID: groupJID, Name: "Alice group contact"},
		{JID: "33655555555@s.whatsapp.net", PushName: "Zed Push"},
		{JID: "", Name: "Alice ghost"},
	})
	e.seedChats(
		wa.Chat{JID: aliceJID, Name: "Alice (chat)"},
		wa.Chat{JID: carolJID, Name: "Carol Danvers"},
		wa.Chat{JID: "120363000000000002@g.us", Name: "Alice and friends", IsGroup: true},
		wa.Chat{JID: bobJID, Name: ""},
	)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"name", "smith", []string{aliceJID}},
		{"name ignoring case", "ALICE", []string{aliceJID}},
		{"push name", "bobby", []string{bobJID}},
		{"store only chat", "danvers", []string{carolJID}},
		{"push name only contact", "zed", []string{"33655555555@s.whatsapp.net"}},
		{"phone digits", "3361111", []string{aliceJID}},
		{"phone with plus and spaces", "+33 6 22 22", []string{bobJID}},
		{"phone of store only chat", "33633333", []string{carolJID}},
		{"groups never match", "friends", nil},
		{"contact without jid ignored", "ghost", nil},
		{"short digit string is not a phone query", "33", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := callAs[contactsOut](e, "search_contacts", map[string]any{"query": tt.query})
			jids := make([]string, len(got.Contacts))
			for i, c := range got.Contacts {
				jids[i] = c.JID
			}
			if !slices.Equal(jids, tt.want) && !(len(jids) == 0 && len(tt.want) == 0) {
				t.Fatalf("query %q -> %v, want %v", tt.query, jids, tt.want)
			}
		})
	}
}

func TestSearchContactsDedupesByJIDPreferringContactName(t *testing.T) {
	e := newEnv(t)
	e.fake.SetContacts([]wa.Contact{
		{JID: aliceJID, Name: "Alice Smith", PushName: "Ali"},
		{JID: bobJID, Name: "", PushName: "Robert"},
	})
	e.seedChats(
		wa.Chat{JID: aliceJID, Name: "Alice Smith"},
		wa.Chat{JID: bobJID, Name: "Bob from chat"},
	)

	got := callAs[contactsOut](e, "search_contacts", map[string]any{"query": "b"})

	if len(got.Contacts) != 1 {
		t.Fatalf("contacts = %+v, want bob once", got.Contacts)
	}
	if c := got.Contacts[0]; c.JID != bobJID || c.Name != "Bob from chat" || c.PushName != "Robert" || c.Phone != "33622222222" {
		t.Fatalf("merged bob = %+v", c)
	}

	all := callAs[contactsOut](e, "search_contacts", map[string]any{"query": "ali"})
	if len(all.Contacts) != 1 || all.Contacts[0].JID != aliceJID || all.Contacts[0].Name != "Alice Smith" || all.Contacts[0].PushName != "Ali" {
		t.Fatalf("alice = %+v", all.Contacts)
	}
}

func TestSearchContactsSortedByNameAndLimited(t *testing.T) {
	e := newEnv(t)
	e.fake.SetContacts([]wa.Contact{
		{JID: carolJID, Name: "ann c"},
		{JID: aliceJID, Name: "Ann A"},
		{JID: bobJID, Name: "ann B"},
	})

	got := callAs[contactsOut](e, "search_contacts", map[string]any{"query": "ann"})
	names := make([]string, len(got.Contacts))
	for i, c := range got.Contacts {
		names[i] = c.Name
	}
	if want := []string{"Ann A", "ann B", "ann c"}; !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}

	limited := callAs[contactsOut](e, "search_contacts", map[string]any{"query": "ann", "limit": 2})
	if len(limited.Contacts) != 2 || limited.Contacts[0].Name != "Ann A" {
		t.Fatalf("limited = %+v", limited.Contacts)
	}
}

func TestSearchContactsNoMatchIsEmptyArray(t *testing.T) {
	e := newEnv(t)

	raw := rawJSON(t, e.ok("search_contacts", map[string]any{"query": "nobody"}).StructuredContent)

	if !strings.Contains(raw, `"contacts":[]`) {
		t.Fatalf("result = %s, want an empty contacts array", raw)
	}
}
