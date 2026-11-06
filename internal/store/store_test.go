package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/store"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

const (
	alice = "33612345678@s.whatsapp.net"
	bob   = "447911123456@s.whatsapp.net"
	group = "123456789-1600000000@g.us"
)

var t0 = time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)

func openStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "messages.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func msg(chat, id string, at time.Time, text string) wa.Message {
	return wa.Message{ID: id, ChatJID: chat, SenderJID: chat, Timestamp: at, Text: text}
}

func save(t *testing.T, s *store.Store, msgs ...wa.Message) {
	t.Helper()
	for _, m := range msgs {
		if err := s.SaveMessage(context.Background(), m); err != nil {
			t.Fatalf("SaveMessage(%s/%s): %v", m.ChatJID, m.ID, err)
		}
	}
}

func ids(msgs []wa.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.ID
	}
	return out
}

func jids(chats []store.ChatSummary) []string {
	out := make([]string, len(chats))
	for i, c := range chats {
		out[i] = c.JID
	}
	return out
}

func listMessages(t *testing.T, s *store.Store, q store.MessageQuery) ([]wa.Message, bool) {
	t.Helper()
	msgs, more, err := s.ListMessages(context.Background(), q)
	if err != nil {
		t.Fatalf("ListMessages(%+v): %v", q, err)
	}
	return msgs, more
}

func listChats(t *testing.T, s *store.Store, q store.ChatQuery) ([]store.ChatSummary, bool) {
	t.Helper()
	chats, more, err := s.ListChats(context.Background(), q)
	if err != nil {
		t.Fatalf("ListChats(%+v): %v", q, err)
	}
	return chats, more
}

func assertIDs(t *testing.T, got []wa.Message, want ...string) {
	t.Helper()
	if !slices.Equal(ids(got), want) {
		t.Errorf("message ids = %v, want %v", ids(got), want)
	}
}

func TestOpenCreatesPrivateFiles(t *testing.T) {
	t.Parallel()
	s, path := openStore(t)
	save(t, s, msg(alice, "m1", t0, "hello"))

	checked := 0
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		checked++
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 600", filepath.Base(p), got)
		}
	}
	if checked == 0 {
		t.Fatal("database file was not created")
	}
}

func TestOpenTightensPermissiveExistingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "messages.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
}

func TestOpenFailsWhenDirectoryIsMissing(t *testing.T) {
	t.Parallel()
	if s, err := store.Open(filepath.Join(t.TempDir(), "missing", "messages.db")); err == nil {
		_ = s.Close()
		t.Fatal("Open must fail when the parent directory does not exist")
	}
}

func TestOpenRejectsNewerSchemaVersion(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "messages.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err := store.Open(path); err == nil {
		_ = s.Close()
		t.Fatal("Open must refuse a database written by a newer schema version")
	}
}

func TestOpenHandlesSpecialCharactersInPath(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "odd dir 100%25 a#b")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "messages.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	save(t, s, msg(alice, "m1", t0, "hello"))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("database must live exactly at the requested path: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "messages.db") {
			t.Errorf("stray file %q created next to the database", e.Name())
		}
	}
	s, err = store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	if _, err := s.GetMessage(context.Background(), alice, "m1"); err != nil {
		t.Errorf("message not persisted: %v", err)
	}
}

func TestDataSurvivesCloseAndReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "messages.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertChats(context.Background(), []wa.Chat{{JID: alice, Name: "Alice"}}); err != nil {
		t.Fatal(err)
	}
	save(t, s, msg(alice, "m1", t0, "hello"))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	chat, err := s.GetChat(context.Background(), alice)
	if err != nil || chat.Name != "Alice" || chat.LastMessageText != "hello" {
		t.Errorf("GetChat = %+v, %v", chat, err)
	}
}

func TestOperationsFailAfterClose(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx := context.Background()
	if err := s.SaveMessage(ctx, msg(alice, "m1", t0, "x")); err == nil {
		t.Error("SaveMessage after Close must fail")
	}
	if _, _, err := s.ListChats(ctx, store.ChatQuery{}); err == nil {
		t.Error("ListChats after Close must fail")
	}
	if _, _, err := s.ListMessages(ctx, store.MessageQuery{}); err == nil {
		t.Error("ListMessages after Close must fail")
	}
}

func TestCancelledContextFailsQueries(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	save(t, s, msg(alice, "m1", t0, "x"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.ListMessages(ctx, store.MessageQuery{}); !errors.Is(err, context.Canceled) {
		t.Errorf("ListMessages error = %v, want context.Canceled", err)
	}
	if err := s.SaveMessage(ctx, msg(alice, "m2", t0, "x")); !errors.Is(err, context.Canceled) {
		t.Errorf("SaveMessage error = %v, want context.Canceled", err)
	}
}

func TestSaveMessageIsAnIdempotentUpsert(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	m := msg(alice, "m1", t0, "first")
	save(t, s, m, m, m)

	msgs, _ := listMessages(t, s, store.MessageQuery{})
	if len(msgs) != 1 {
		t.Fatalf("saving the same message three times left %d rows", len(msgs))
	}

	m.Text = "edited"
	m.FromMe = true
	m.Timestamp = t0.Add(time.Minute)
	save(t, s, m)
	got, err := s.GetMessage(context.Background(), alice, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "edited" || !got.FromMe || !got.Timestamp.Equal(t0.Add(time.Minute)) {
		t.Errorf("upsert did not update the row: %+v", got)
	}
	if n, _ := listMessages(t, s, store.MessageQuery{}); len(n) != 1 {
		t.Errorf("update created a second row: %v", ids(n))
	}
}

func TestSaveMessageKeepsSenderNameWhenNewCopyHasNone(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	m := msg(group, "m1", t0, "hi")
	m.SenderJID = alice
	m.SenderName = "Alice"
	save(t, s, m)
	m.SenderName = ""
	save(t, s, m)
	got, err := s.GetMessage(context.Background(), group, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.SenderName != "Alice" {
		t.Errorf("SenderName = %q, want Alice to survive an empty update", got.SenderName)
	}
	m.SenderName = "Alice B"
	save(t, s, m)
	if got, _ = s.GetMessage(context.Background(), group, "m1"); got.SenderName != "Alice B" {
		t.Errorf("SenderName = %q, want a non-empty update to apply", got.SenderName)
	}
}

func TestSameMessageIDInDifferentChatsAreDistinctRows(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	save(t, s, msg(alice, "dup", t0, "to alice"), msg(bob, "dup", t0, "to bob"))
	msgs, _ := listMessages(t, s, store.MessageQuery{})
	if len(msgs) != 2 {
		t.Fatalf("got %d rows, want 2", len(msgs))
	}
}

func TestSaveMessageRequiresChatAndID(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	secret := "very secret body text"
	for _, m := range []wa.Message{
		{ChatJID: alice, Timestamp: t0, Text: secret},
		{ID: "m1", Timestamp: t0, Text: secret},
	} {
		err := s.SaveMessage(context.Background(), m)
		if err == nil {
			t.Errorf("SaveMessage(%+v) must fail", m)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error %q leaks the message body", err)
		}
	}
	if msgs, _ := listMessages(t, s, store.MessageQuery{}); len(msgs) != 0 {
		t.Errorf("rejected messages were stored: %v", ids(msgs))
	}
	if chats, _ := listChats(t, s, store.ChatQuery{}); len(chats) != 0 {
		t.Errorf("rejected messages created chats: %v", jids(chats))
	}
}

func TestSaveMessageCreatesChatAndTracksLatestTimestamp(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	save(t, s, msg(group, "m2", t0.Add(2*time.Hour), "newest"))
	save(t, s, msg(group, "m1", t0, "older arrives later"))

	chat, err := s.GetChat(context.Background(), group)
	if err != nil {
		t.Fatal(err)
	}
	if !chat.IsGroup {
		t.Error("a g.us chat created from a message must be a group")
	}
	if !chat.LastMessageTime.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("LastMessageTime = %v, an older message must not move it back", chat.LastMessageTime)
	}
	if chat.LastMessageText != "newest" {
		t.Errorf("LastMessageText = %q, want the newest message by timestamp", chat.LastMessageText)
	}
	save(t, s, msg(alice, "d1", t0, "x"))
	if c, _ := s.GetChat(context.Background(), alice); c.IsGroup {
		t.Error("a user chat must not be flagged as a group")
	}
}

func TestSaveMessageDoesNotBlankExistingChatName(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	if err := s.UpsertChats(context.Background(), []wa.Chat{{JID: group, Name: "Family", IsGroup: true}}); err != nil {
		t.Fatal(err)
	}
	save(t, s, msg(group, "m1", t0, "hello"))
	chat, err := s.GetChat(context.Background(), group)
	if err != nil || chat.Name != "Family" {
		t.Errorf("GetChat = %+v, %v; name must survive SaveMessage", chat, err)
	}
}

func TestUpsertChatsNeverOverwritesNameWithEmpty(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	ctx := context.Background()
	steps := []struct {
		in   wa.Chat
		want string
	}{
		{wa.Chat{JID: alice, Name: "Alice"}, "Alice"},
		{wa.Chat{JID: alice, Name: ""}, "Alice"},
		{wa.Chat{JID: alice, Name: "Alice Martin"}, "Alice Martin"},
		{wa.Chat{JID: alice}, "Alice Martin"},
	}
	for i, st := range steps {
		if err := s.UpsertChats(ctx, []wa.Chat{st.in}); err != nil {
			t.Fatal(err)
		}
		c, err := s.GetChat(ctx, alice)
		if err != nil || c.Name != st.want {
			t.Fatalf("step %d: name = %q (%v), want %q", i, c.Name, err, st.want)
		}
	}
	if err := s.UpsertChats(ctx, []wa.Chat{{JID: group, Name: "", IsGroup: true}}); err != nil {
		t.Fatal(err)
	}
	if c, err := s.GetChat(ctx, group); err != nil || c.Name != "" || !c.IsGroup {
		t.Errorf("new unnamed group = %+v, %v", c, err)
	}
}

func TestUpsertChatsIgnoresEntriesWithoutJID(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	if err := s.UpsertChats(context.Background(), []wa.Chat{{Name: "ghost"}, {JID: alice, Name: "Alice"}}); err != nil {
		t.Fatal(err)
	}
	chats, _ := listChats(t, s, store.ChatQuery{})
	if !slices.Equal(jids(chats), []string{alice}) {
		t.Errorf("chats = %v, want only alice", jids(chats))
	}
}

func TestSaveHistoryStoresChatsAndMessagesIdempotently(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	ctx := context.Background()
	chats := []wa.Chat{{JID: alice, Name: "Alice"}, {JID: group, Name: "Family", IsGroup: true}}
	msgs := []wa.Message{
		msg(alice, "a1", t0, "one"),
		msg(alice, "a2", t0.Add(time.Minute), "two"),
		msg(group, "g1", t0.Add(2*time.Minute), "three"),
	}
	for range 2 {
		if err := s.SaveHistory(ctx, chats, msgs); err != nil {
			t.Fatalf("SaveHistory: %v", err)
		}
	}
	got, _ := listMessages(t, s, store.MessageQuery{Oldest: true})
	assertIDs(t, got, "a1", "a2", "g1")
	gotChats, _ := listChats(t, s, store.ChatQuery{})
	if !slices.Equal(jids(gotChats), []string{group, alice}) {
		t.Fatalf("chats = %v, want [group alice] by last activity", jids(gotChats))
	}
	if gotChats[0].Name != "Family" || !gotChats[0].IsGroup {
		t.Errorf("group chat = %+v", gotChats[0])
	}
}

func TestSaveHistoryIsAtomic(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	err := s.SaveHistory(context.Background(),
		[]wa.Chat{{JID: alice, Name: "Alice"}},
		[]wa.Message{msg(alice, "ok", t0, "fine"), {ChatJID: alice, Timestamp: t0, Text: "no id"}},
	)
	if err == nil {
		t.Fatal("SaveHistory with an invalid message must fail")
	}
	if msgs, _ := listMessages(t, s, store.MessageQuery{}); len(msgs) != 0 {
		t.Errorf("failed batch left messages behind: %v", ids(msgs))
	}
	if chats, _ := listChats(t, s, store.ChatQuery{}); len(chats) != 0 {
		t.Errorf("failed batch left chats behind: %v", jids(chats))
	}
}

func TestListChatsSortsByLastActivityNewestFirst(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	ctx := context.Background()
	if err := s.UpsertChats(ctx, []wa.Chat{{JID: "empty@s.whatsapp.net", Name: "No messages"}}); err != nil {
		t.Fatal(err)
	}
	save(t, s,
		msg(alice, "a1", t0, "old"),
		msg(bob, "b1", t0.Add(time.Hour), "newest"),
		msg(group, "g1", t0.Add(30*time.Minute), "middle"),
	)
	chats, more := listChats(t, s, store.ChatQuery{})
	want := []string{bob, group, alice, "empty@s.whatsapp.net"}
	if !slices.Equal(jids(chats), want) || more {
		t.Fatalf("chats = %v more=%v, want %v", jids(chats), more, want)
	}
	if chats[0].LastMessageText != "newest" || !chats[0].LastMessageTime.Equal(t0.Add(time.Hour)) {
		t.Errorf("preview = %+v", chats[0])
	}
	if !chats[3].LastMessageTime.IsZero() || chats[3].LastMessageText != "" {
		t.Errorf("chat without messages = %+v", chats[3])
	}
}

func TestListChatsPreviewDescribesTheLatestMessage(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	older := msg(group, "m1", t0, "older")
	older.SenderJID = alice
	newer := msg(group, "m2", t0.Add(time.Minute), "mine")
	newer.SenderJID = bob
	newer.FromMe = true
	save(t, s, newer, older)

	c, err := s.GetChat(context.Background(), group)
	if err != nil {
		t.Fatal(err)
	}
	if c.LastMessageText != "mine" || !c.LastMessageFromMe || c.LastSenderJID != bob {
		t.Errorf("preview = %+v", c)
	}
	chats, _ := listChats(t, s, store.ChatQuery{})
	if len(chats) != 1 || chats[0].LastMessageText != "mine" || chats[0].LastSenderJID != bob {
		t.Errorf("ListChats preview = %+v", chats)
	}
}

func TestListChatsSortByName(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	if err := s.UpsertChats(context.Background(), []wa.Chat{
		{JID: "1@s.whatsapp.net", Name: "charlie"},
		{JID: "2@s.whatsapp.net", Name: "Alice"},
		{JID: "3@s.whatsapp.net", Name: "bob"},
		{JID: "0000@s.whatsapp.net", Name: ""},
		{JID: "zzz@s.whatsapp.net", Name: ""},
	}); err != nil {
		t.Fatal(err)
	}
	chats, _ := listChats(t, s, store.ChatQuery{SortBy: "name"})
	want := []string{"0000@s.whatsapp.net", "2@s.whatsapp.net", "3@s.whatsapp.net", "1@s.whatsapp.net", "zzz@s.whatsapp.net"}
	if !slices.Equal(jids(chats), want) {
		t.Errorf("by name = %v, want %v (case-insensitive, unnamed chats sort by jid)", jids(chats), want)
	}
	if def, _ := listChats(t, s, store.ChatQuery{SortBy: "bogus"}); len(def) != 5 {
		t.Errorf("unknown sort must fall back to the default order, got %d chats", len(def))
	}
}

func TestListChatsQueryMatchesNameOrJIDCaseInsensitively(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	if err := s.UpsertChats(context.Background(), []wa.Chat{
		{JID: alice, Name: "Alice Martin"},
		{JID: bob, Name: "Bob"},
		{JID: group, Name: "Family 100%", IsGroup: true},
		{JID: "999@s.whatsapp.net", Name: "Snake_case"},
		{JID: "888@s.whatsapp.net", Name: "SnakeXcase"},
	}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		query string
		want  []string
	}{
		{"alice", []string{alice}},
		{"ALICE MAR", []string{alice}},
		{"mART", []string{alice}},
		{"3361234", []string{alice}},
		{"S.WHATSAPP.NET", []string{alice, bob, "888@s.whatsapp.net", "999@s.whatsapp.net"}},
		{"g.us", []string{group}},
		{"nobody", nil},
		{"%", []string{group}},
		{"100%", []string{group}},
		{"_", []string{"999@s.whatsapp.net"}},
		{"snake_case", []string{"999@s.whatsapp.net"}},
		{`\`, nil},
		{"' OR '1'='1", nil},
	}
	for _, tt := range tests {
		chats, _ := listChats(t, s, store.ChatQuery{Query: tt.query, SortBy: "name"})
		got := jids(chats)
		slices.Sort(got)
		want := slices.Clone(tt.want)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("query %q -> %v, want %v", tt.query, got, want)
		}
	}
}

func TestListChatsPagination(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	for i := range 5 {
		save(t, s, msg(fmt.Sprintf("%d@s.whatsapp.net", 100+i), "m", t0.Add(time.Duration(i)*time.Minute), "x"))
	}
	order := []string{"104@s.whatsapp.net", "103@s.whatsapp.net", "102@s.whatsapp.net", "101@s.whatsapp.net", "100@s.whatsapp.net"}
	tests := []struct {
		name     string
		q        store.ChatQuery
		want     []string
		wantMore bool
	}{
		{"first page", store.ChatQuery{Limit: 2}, order[:2], true},
		{"middle page", store.ChatQuery{Limit: 2, Offset: 2}, order[2:4], true},
		{"last partial page", store.ChatQuery{Limit: 2, Offset: 4}, order[4:], false},
		{"exact fit", store.ChatQuery{Limit: 5}, order, false},
		{"one short", store.ChatQuery{Limit: 4}, order[:4], true},
		{"offset past the end", store.ChatQuery{Limit: 2, Offset: 10}, nil, false},
		{"zero limit defaults to 20", store.ChatQuery{}, order, false},
		{"negative offset treated as zero", store.ChatQuery{Limit: 2, Offset: -3}, order[:2], true},
		{"negative limit clamps to one", store.ChatQuery{Limit: -5}, order[:1], true},
	}
	for _, tt := range tests {
		got, more := listChats(t, s, tt.q)
		if !slices.Equal(jids(got), tt.want) || more != tt.wantMore {
			t.Errorf("%s: got %v more=%v, want %v more=%v", tt.name, jids(got), more, tt.want, tt.wantMore)
		}
	}
}

func TestListChatsLimitIsClampedTo200(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	chats := make([]wa.Chat, 205)
	for i := range chats {
		chats[i] = wa.Chat{JID: fmt.Sprintf("%04d@s.whatsapp.net", i), Name: fmt.Sprintf("c%d", i)}
	}
	if err := s.UpsertChats(context.Background(), chats); err != nil {
		t.Fatal(err)
	}
	got, more := listChats(t, s, store.ChatQuery{Limit: 100000})
	if len(got) != 200 || !more {
		t.Errorf("limit 100000 returned %d chats more=%v, want 200 and true", len(got), more)
	}
	got, more = listChats(t, s, store.ChatQuery{Limit: 200, Offset: 200})
	if len(got) != 5 || more {
		t.Errorf("tail returned %d chats more=%v, want 5 and false", len(got), more)
	}
}

func TestGetChat(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	if _, err := s.GetChat(context.Background(), alice); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetChat on an empty store = %v, want ErrNotFound", err)
	}
	if err := s.UpsertChats(context.Background(), []wa.Chat{{JID: alice, Name: "Alice"}}); err != nil {
		t.Fatal(err)
	}
	c, err := s.GetChat(context.Background(), alice)
	if err != nil || c.JID != alice || c.Name != "Alice" || c.IsGroup {
		t.Errorf("GetChat = %+v, %v", c, err)
	}
	if _, err := s.GetChat(context.Background(), bob); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetChat(unknown) = %v, want ErrNotFound", err)
	}
}

func seedConversation(t *testing.T) *store.Store {
	t.Helper()
	s, _ := openStore(t)
	mk := func(chat, id, sender string, off int, fromMe bool, text string) wa.Message {
		m := msg(chat, id, t0.Add(time.Duration(off)*time.Minute), text)
		m.SenderJID = sender
		m.FromMe = fromMe
		return m
	}
	save(t, s,
		mk(alice, "a1", alice, 0, false, "Hello World"),
		mk(alice, "a2", "", 1, true, "hello again"),
		mk(alice, "a3", alice, 2, false, "100% sure"),
		mk(alice, "a4", alice, 3, false, "100 percent"),
		mk(group, "g1", bob, 4, false, "snake_case name"),
		mk(group, "g2", alice, 5, false, "snakeXcase name"),
		mk(group, "g3", bob, 6, false, `back\slash path`),
		mk(group, "g4", bob, 7, false, "plain"),
	)
	return s
}

func TestListMessagesNewestFirstByDefaultAndOldestOnRequest(t *testing.T) {
	t.Parallel()
	s := seedConversation(t)
	got, more := listMessages(t, s, store.MessageQuery{})
	assertIDs(t, got, "g4", "g3", "g2", "g1", "a4", "a3", "a2", "a1")
	if more {
		t.Error("8 messages fit the default limit of 20")
	}
	got, _ = listMessages(t, s, store.MessageQuery{Oldest: true})
	assertIDs(t, got, "a1", "a2", "a3", "a4", "g1", "g2", "g3", "g4")
}

func TestListMessagesBreaksTimestampTiesByID(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	save(t, s, msg(alice, "b", t0, ""), msg(alice, "a", t0, ""), msg(alice, "c", t0, ""))
	got, _ := listMessages(t, s, store.MessageQuery{})
	assertIDs(t, got, "c", "b", "a")
	got, _ = listMessages(t, s, store.MessageQuery{Oldest: true})
	assertIDs(t, got, "a", "b", "c")
}

func TestListMessagesFilters(t *testing.T) {
	t.Parallel()
	s := seedConversation(t)
	tests := []struct {
		name string
		q    store.MessageQuery
		want []string
	}{
		{"by chat", store.MessageQuery{ChatJID: alice}, []string{"a4", "a3", "a2", "a1"}},
		{"by other chat", store.MessageQuery{ChatJID: group}, []string{"g4", "g3", "g2", "g1"}},
		{"unknown chat", store.MessageQuery{ChatJID: "nobody@s.whatsapp.net"}, nil},
		{"by sender", store.MessageQuery{SenderJID: bob}, []string{"g4", "g3", "g1"}},
		{"by sender in chat", store.MessageQuery{ChatJID: group, SenderJID: alice}, []string{"g2"}},
		{"sender is an exact match", store.MessageQuery{SenderJID: "447911123456"}, nil},
		{"text case-insensitive upper", store.MessageQuery{Text: "HELLO"}, []string{"a2", "a1"}},
		{"text case-insensitive lower", store.MessageQuery{Text: "world"}, []string{"a1"}},
		{"text substring", store.MessageQuery{Text: "ello wor"}, []string{"a1"}},
		{"text with chat", store.MessageQuery{ChatJID: group, Text: "name"}, []string{"g2", "g1"}},
		{"text no match", store.MessageQuery{Text: "absent"}, nil},
		{"percent is literal", store.MessageQuery{Text: "%"}, []string{"a3"}},
		{"percent suffix is literal", store.MessageQuery{Text: "100%"}, []string{"a3"}},
		{"percent in middle is literal", store.MessageQuery{Text: "0% s"}, []string{"a3"}},
		{"underscore is literal", store.MessageQuery{Text: "_"}, []string{"g1"}},
		{"underscore between letters is literal", store.MessageQuery{Text: "snake_case"}, []string{"g1"}},
		{"backslash is literal", store.MessageQuery{Text: `\`}, []string{"g3"}},
		{"backslash then letter is literal", store.MessageQuery{Text: `k\s`}, []string{"g3"}},
		{"sql metacharacters are data", store.MessageQuery{Text: "'; DROP TABLE messages;--"}, nil},
		{"sql injection in chat jid", store.MessageQuery{ChatJID: "x' OR '1'='1"}, nil},
		{"sql injection in sender jid", store.MessageQuery{SenderJID: "' OR 1=1 --"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, _ := listMessages(t, s, tt.q)
			assertIDs(t, got, tt.want...)
		})
	}
}

func TestListMessagesInjectionAttemptsDoNotDamageTheTable(t *testing.T) {
	t.Parallel()
	s := seedConversation(t)
	for _, text := range []string{"'; DROP TABLE messages;--", "x' OR '1'='1", "\"; DELETE FROM messages; --"} {
		listMessages(t, s, store.MessageQuery{Text: text, ChatJID: text, SenderJID: text})
	}
	if all, _ := listMessages(t, s, store.MessageQuery{}); len(all) != 8 {
		t.Errorf("injection attempts damaged the table: %d rows remain, want 8", len(all))
	}
}

func TestListMessagesTimeBounds(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	for i := range 5 {
		save(t, s, msg(alice, fmt.Sprintf("m%d", i), t0.Add(time.Duration(i)*time.Hour), "x"))
	}
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Hour) }
	tests := []struct {
		name string
		q    store.MessageQuery
		want []string
	}{
		{"after is inclusive", store.MessageQuery{After: at(2)}, []string{"m4", "m3", "m2"}},
		{"before is exclusive", store.MessageQuery{Before: at(2)}, []string{"m1", "m0"}},
		{"window", store.MessageQuery{After: at(1), Before: at(3)}, []string{"m2", "m1"}},
		{"after one nanosecond later excludes", store.MessageQuery{After: at(2).Add(time.Nanosecond)}, []string{"m4", "m3"}},
		{"before one nanosecond later includes", store.MessageQuery{Before: at(2).Add(time.Nanosecond)}, []string{"m2", "m1", "m0"}},
		{"empty window", store.MessageQuery{After: at(2), Before: at(2)}, nil},
		{"inverted window", store.MessageQuery{After: at(3), Before: at(1)}, nil},
		{"after the end", store.MessageQuery{After: at(10)}, nil},
		{"before the start", store.MessageQuery{Before: at(0)}, nil},
		{"non-UTC bound", store.MessageQuery{After: at(3).In(time.FixedZone("x", 5*3600))}, []string{"m4", "m3"}},
		{"window oldest first", store.MessageQuery{After: at(1), Before: at(4), Oldest: true}, []string{"m1", "m2", "m3"}},
	}
	for _, tt := range tests {
		got, _ := listMessages(t, s, tt.q)
		if !slices.Equal(ids(got), tt.want) {
			t.Errorf("%s: got %v, want %v", tt.name, ids(got), tt.want)
		}
	}
}

func TestListMessagesPaginationAndClamps(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	const total = 205
	batch := make([]wa.Message, total)
	for i := range batch {
		batch[i] = msg(alice, fmt.Sprintf("m%03d", i), t0.Add(time.Duration(i)*time.Second), "x")
	}
	if err := s.SaveHistory(context.Background(), nil, batch); err != nil {
		t.Fatal(err)
	}

	page := func(q store.MessageQuery) ([]string, bool) {
		got, more := listMessages(t, s, q)
		return ids(got), more
	}

	got, more := page(store.MessageQuery{Oldest: true, Limit: 3})
	if !slices.Equal(got, []string{"m000", "m001", "m002"}) || !more {
		t.Errorf("page 1 = %v more=%v", got, more)
	}
	got, more = page(store.MessageQuery{Oldest: true, Limit: 3, Offset: 3})
	if !slices.Equal(got, []string{"m003", "m004", "m005"}) || !more {
		t.Errorf("page 2 = %v more=%v", got, more)
	}
	got, more = page(store.MessageQuery{Oldest: true, Limit: 10, Offset: 200})
	if !slices.Equal(got, []string{"m200", "m201", "m202", "m203", "m204"}) || more {
		t.Errorf("last page = %v more=%v", got, more)
	}
	if got, more = page(store.MessageQuery{Limit: 5, Offset: 205}); len(got) != 0 || more {
		t.Errorf("offset at the end = %v more=%v", got, more)
	}
	if got, more = page(store.MessageQuery{Limit: total, Offset: 0}); len(got) != 200 || !more {
		t.Errorf("limit above the cap returned %d more=%v, want 200 and true", len(got), more)
	}
	if got, more = page(store.MessageQuery{Limit: 200, Offset: 5}); len(got) != 200 || more {
		t.Errorf("exact tail returned %d more=%v, want 200 and false", len(got), more)
	}
	if got, more = page(store.MessageQuery{}); len(got) != 20 || !more {
		t.Errorf("default limit returned %d more=%v, want 20 and true", len(got), more)
	}
	if got, _ = page(store.MessageQuery{Limit: -1}); len(got) != 1 {
		t.Errorf("negative limit returned %d messages, want clamp to 1", len(got))
	}
	if got, _ = page(store.MessageQuery{Limit: 2, Offset: -9}); !slices.Equal(got, []string{"m204", "m203"}) {
		t.Errorf("negative offset = %v, want treated as 0", got)
	}
}

func TestMediaRoundTrip(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	media := &wa.Media{
		Type: wa.MediaDocument, MimeType: "application/pdf", FileName: "report q1.pdf",
		DirectPath: "/v/t62/xyz?ccb=1", URL: "https://mmg.whatsapp.net/v/x",
		MediaKey: []byte{0, 1, 2, 0xff}, FileSHA256: bytes.Repeat([]byte{0xab}, 32), FileEncSHA256: bytes.Repeat([]byte{0xcd}, 32),
		FileLength: 1<<40 + 7,
	}
	in := msg(alice, "media1", t0, "see attached")
	in.Media = media
	text := msg(alice, "text1", t0.Add(time.Second), "plain")
	save(t, s, in, text)

	got, err := s.GetMessage(context.Background(), alice, "media1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Media == nil {
		t.Fatal("media lost")
	}
	m := *got.Media
	if m.Type != media.Type || m.MimeType != media.MimeType || m.FileName != media.FileName ||
		m.DirectPath != media.DirectPath || m.URL != media.URL || m.FileLength != media.FileLength {
		t.Errorf("media scalars = %+v, want %+v", m, *media)
	}
	if !bytes.Equal(m.MediaKey, media.MediaKey) || !bytes.Equal(m.FileSHA256, media.FileSHA256) || !bytes.Equal(m.FileEncSHA256, media.FileEncSHA256) {
		t.Errorf("media blobs differ: %+v", m)
	}
	if got.Text != "see attached" {
		t.Errorf("caption = %q", got.Text)
	}

	plain, err := s.GetMessage(context.Background(), alice, "text1")
	if err != nil || plain.Media != nil {
		t.Errorf("text-only message = %+v, %v; want nil Media", plain, err)
	}

	listed, _ := listMessages(t, s, store.MessageQuery{Oldest: true})
	if len(listed) != 2 || listed[0].Media == nil || listed[0].Media.DirectPath != media.DirectPath || listed[1].Media != nil {
		t.Errorf("ListMessages media = %+v", listed)
	}

	in.Media = nil
	save(t, s, in)
	if got, _ = s.GetMessage(context.Background(), alice, "media1"); got.Media != nil {
		t.Errorf("re-saving without media must clear it, got %+v", got.Media)
	}
}

func TestMessageFieldsAndTimestampRoundTrip(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	local := time.Date(2026, 5, 6, 7, 8, 9, 123456789, time.FixedZone("CEST", 2*3600))
	in := wa.Message{
		ID: "m1", ChatJID: group, SenderJID: alice, SenderName: "Alice", Timestamp: local, FromMe: true,
		Text: "héllo 😀 \"quoted\" 'single' \n newline",
	}
	save(t, s, in)
	got, err := s.GetMessage(context.Background(), group, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != in.ID || got.ChatJID != in.ChatJID || got.SenderJID != in.SenderJID || got.SenderName != in.SenderName ||
		got.FromMe != in.FromMe || got.Text != in.Text {
		t.Errorf("got %+v, want %+v", got, in)
	}
	if !got.Timestamp.Equal(local) {
		t.Errorf("Timestamp = %v, want the same instant as %v", got.Timestamp, local)
	}
	if got.Timestamp.Location() != time.UTC {
		t.Errorf("Timestamp location = %v, want UTC", got.Timestamp.Location())
	}
}

func TestGetMessage(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	save(t, s,
		msg(alice, "unique", t0, "only here"),
		msg(alice, "dup", t0, "alice copy"),
		msg(bob, "dup", t0, "bob copy"),
	)
	ctx := context.Background()

	if m, err := s.GetMessage(ctx, alice, "unique"); err != nil || m.Text != "only here" {
		t.Errorf("GetMessage(chat, id) = %+v, %v", m, err)
	}
	if m, err := s.GetMessage(ctx, "", "unique"); err != nil || m.ChatJID != alice {
		t.Errorf("GetMessage(any chat, unique id) = %+v, %v", m, err)
	}
	if _, err := s.GetMessage(ctx, "", "dup"); !errors.Is(err, store.ErrAmbiguous) {
		t.Errorf("GetMessage(any chat, shared id) = %v, want ErrAmbiguous", err)
	}
	if m, err := s.GetMessage(ctx, bob, "dup"); err != nil || m.Text != "bob copy" {
		t.Errorf("GetMessage(bob, dup) = %+v, %v", m, err)
	}
	for _, c := range []struct{ chat, id string }{{alice, "missing"}, {"", "missing"}, {group, "unique"}, {bob, "unique"}} {
		if _, err := s.GetMessage(ctx, c.chat, c.id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetMessage(%q, %q) = %v, want ErrNotFound", c.chat, c.id, err)
		}
	}
	if errors.Is(store.ErrAmbiguous, store.ErrNotFound) {
		t.Error("ErrAmbiguous and ErrNotFound must be distinct")
	}

	chats, err := s.MessageChats(ctx, "dup")
	if err != nil || !slices.Equal(chats, []string{alice, bob}) {
		t.Errorf("MessageChats(dup) = %v, %v; want both chats sorted", chats, err)
	}
	if chats, err = s.MessageChats(ctx, "missing"); err != nil || len(chats) != 0 {
		t.Errorf("MessageChats(missing) = %v, %v", chats, err)
	}
}

func seedContext(t *testing.T, n int) *store.Store {
	t.Helper()
	s, _ := openStore(t)
	for i := range n {
		save(t, s,
			msg(alice, fmt.Sprintf("a%02d", i), t0.Add(time.Duration(i)*time.Minute), "x"),
			msg(bob, fmt.Sprintf("b%02d", i), t0.Add(time.Duration(i)*time.Minute+30*time.Second), "other chat"),
		)
	}
	return s
}

func TestMessageContextReturnsNeighboursInChronologicalOrder(t *testing.T) {
	t.Parallel()
	s := seedContext(t, 10)
	older, target, newer, err := s.MessageContext(context.Background(), alice, "a05", 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if target.ID != "a05" {
		t.Errorf("target = %+v", target)
	}
	assertIDs(t, older, "a02", "a03", "a04")
	assertIDs(t, newer, "a06", "a07")
}

func TestMessageContextStaysInsideTheChat(t *testing.T) {
	t.Parallel()
	s := seedContext(t, 10)
	older, _, newer, err := s.MessageContext(context.Background(), "", "a05", 50, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range append(slices.Clone(older), newer...) {
		if m.ChatJID != alice {
			t.Errorf("context leaked message %s from %s", m.ID, m.ChatJID)
		}
	}
	if len(older) != 5 || len(newer) != 4 {
		t.Errorf("got %d older and %d newer, want 5 and 4", len(older), len(newer))
	}
}

func TestMessageContextEdgesAndZeroCounts(t *testing.T) {
	t.Parallel()
	s := seedContext(t, 5)
	ctx := context.Background()

	older, target, newer, err := s.MessageContext(ctx, alice, "a00", 5, 1)
	if err != nil || target.ID != "a00" || len(older) != 0 {
		t.Errorf("first message: older=%v target=%v err=%v", ids(older), target.ID, err)
	}
	assertIDs(t, newer, "a01")

	older, _, newer, err = s.MessageContext(ctx, alice, "a04", 1, 5)
	if err != nil || len(newer) != 0 {
		t.Errorf("last message: newer=%v err=%v", ids(newer), err)
	}
	assertIDs(t, older, "a03")

	older, target, newer, err = s.MessageContext(ctx, alice, "a02", 0, 0)
	if err != nil || target.ID != "a02" || len(older) != 0 || len(newer) != 0 {
		t.Errorf("zero counts: older=%v newer=%v err=%v", ids(older), ids(newer), err)
	}
	older, _, newer, err = s.MessageContext(ctx, alice, "a02", -4, -4)
	if err != nil || len(older) != 0 || len(newer) != 0 {
		t.Errorf("negative counts must clamp to zero: older=%v newer=%v err=%v", ids(older), ids(newer), err)
	}
}

func TestMessageContextClampsCountsTo50(t *testing.T) {
	t.Parallel()
	s := seedContext(t, 130)
	older, _, newer, err := s.MessageContext(context.Background(), alice, "a65", 1000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(older) != 50 || len(newer) != 50 {
		t.Fatalf("got %d older and %d newer, want 50 each", len(older), len(newer))
	}
	if older[0].ID != "a15" || older[49].ID != "a64" || newer[0].ID != "a66" || newer[49].ID != "a115" {
		t.Errorf("older %s..%s newer %s..%s", older[0].ID, older[49].ID, newer[0].ID, newer[49].ID)
	}
}

func TestMessageContextOrdersTimestampTiesByID(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	save(t, s,
		msg(alice, "c", t0, ""), msg(alice, "a", t0, ""), msg(alice, "b", t0, ""),
		msg(alice, "before", t0.Add(-time.Minute), ""), msg(alice, "after", t0.Add(time.Minute), ""),
	)
	older, target, newer, err := s.MessageContext(context.Background(), alice, "b", 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if target.ID != "b" {
		t.Fatalf("target = %s", target.ID)
	}
	assertIDs(t, older, "before", "a")
	assertIDs(t, newer, "c", "after")
}

func TestMessageContextErrors(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	save(t, s, msg(alice, "dup", t0, ""), msg(bob, "dup", t0, ""))
	ctx := context.Background()
	if _, _, _, err := s.MessageContext(ctx, alice, "missing", 1, 1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing id = %v, want ErrNotFound", err)
	}
	if _, _, _, err := s.MessageContext(ctx, "", "dup", 1, 1); !errors.Is(err, store.ErrAmbiguous) {
		t.Errorf("shared id without chat = %v, want ErrAmbiguous", err)
	}
	if _, target, _, err := s.MessageContext(ctx, bob, "dup", 1, 1); err != nil || target.ChatJID != bob {
		t.Errorf("shared id with chat = %+v, %v", target, err)
	}
}

func TestConcurrentWritersAndReaders(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	const workers, perWorker = 8, 25
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWorker {
				m := msg(alice, fmt.Sprintf("w%d-%d", w, i), t0.Add(time.Duration(w*perWorker+i)*time.Second), "x")
				if err := s.SaveMessage(context.Background(), m); err != nil {
					t.Errorf("SaveMessage: %v", err)
					return
				}
				if _, _, err := s.ListMessages(context.Background(), store.MessageQuery{Limit: 5}); err != nil {
					t.Errorf("ListMessages: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	all, more := listMessages(t, s, store.MessageQuery{Limit: 200})
	if len(all) != workers*perWorker || more {
		t.Errorf("stored %d messages more=%v, want %d", len(all), more, workers*perWorker)
	}
}

func TestSaveMessageIgnoresACopyFromAnotherSender(t *testing.T) {
	t.Parallel()
	s, _ := openStore(t)
	original := msg(group, "m1", t0, "original")
	original.SenderJID = alice
	save(t, s, original)

	forged := original
	forged.SenderJID = bob
	forged.FromMe = true
	forged.Text = "forged"
	forged.Timestamp = t0.Add(time.Hour)
	save(t, s, forged)

	got, err := s.GetMessage(context.Background(), group, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got.SenderJID != alice || got.FromMe || got.Text != "original" || !got.Timestamp.Equal(t0) {
		t.Errorf("row was changed by another sender's copy: %+v", got)
	}
}

func TestApplyEdit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStore(t)
	m := msg(group, "m1", t0, "typo")
	m.SenderJID = alice
	save(t, s, m)
	before, err := s.GetChat(ctx, group)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("wrong sender is ignored", func(t *testing.T) {
		if err := s.ApplyEdit(ctx, group, "m1", bob, "hijacked"); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetMessage(ctx, group, "m1")
		if got.Text != "typo" {
			t.Errorf("text = %q, want unchanged", got.Text)
		}
	})
	t.Run("wrong chat is ignored", func(t *testing.T) {
		if err := s.ApplyEdit(ctx, alice, "m1", alice, "elsewhere"); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetMessage(ctx, group, "m1")
		if got.Text != "typo" {
			t.Errorf("text = %q, want unchanged", got.Text)
		}
	})
	t.Run("unknown message is a no-op", func(t *testing.T) {
		if err := s.ApplyEdit(ctx, group, "missing", alice, "x"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetMessage(ctx, group, "missing"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("edit created a message: %v", err)
		}
	})
	t.Run("matching sender updates text only", func(t *testing.T) {
		if err := s.ApplyEdit(ctx, group, "m1", alice, "fixed"); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetMessage(ctx, group, "m1")
		if got.Text != "fixed" || got.SenderJID != alice || got.FromMe || !got.Timestamp.Equal(t0) {
			t.Errorf("got %+v", got)
		}
		after, _ := s.GetChat(ctx, group)
		if !after.LastMessageTime.Equal(before.LastMessageTime) {
			t.Errorf("edit moved last message time %v -> %v", before.LastMessageTime, after.LastMessageTime)
		}
	})
}

func TestUpdateChatNamesOnlyFillsEmptyNamesOfExistingChats(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStore(t)
	save(t, s, msg(alice, "m1", t0, "hi"))
	if err := s.UpsertChats(ctx, []wa.Chat{{JID: bob, Name: "Bobby"}}); err != nil {
		t.Fatal(err)
	}

	err := s.UpdateChatNames(ctx, []wa.Chat{
		{JID: alice, Name: "Alice A"},
		{JID: bob, Name: "Robert"},
		{JID: "33600000000@s.whatsapp.net", Name: "Stranger"},
		{JID: alice, Name: ""},
	})
	if err != nil {
		t.Fatal(err)
	}

	chats, _, err := s.ListChats(ctx, store.ChatQuery{})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, c := range chats {
		names[c.JID] = c.Name
	}
	if len(names) != 2 || names[alice] != "Alice A" || names[bob] != "Bobby" {
		t.Errorf("chat names = %v, want alice named, bob kept, no new chat", names)
	}
}

func TestHistoryReplayPreservesMessageMutation(t *testing.T) {
	s, _ := openStore(t)
	original := msg(alice, "m", t0, "original")
	original.FromMe = true
	save(t, s, original)
	ctx := context.Background()
	if err := s.MarkEditedOwn(ctx, alice, "m", "edited", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	save(t, s, original)
	got, err := s.GetMessage(ctx, alice, "m")
	if err != nil || !got.Edited || got.Text != "edited" {
		t.Fatalf("replayed edit: %+v %v", got, err)
	}
	if err := s.MarkRevokedOwn(ctx, alice, "m", t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	save(t, s, original)
	got, err = s.GetMessage(ctx, alice, "m")
	if err != nil || !got.Revoked || !got.Edited || got.Text != "edited" || !got.Timestamp.Equal(t0) {
		t.Fatalf("replayed revoke: %+v %v", got, err)
	}
}
