package tools_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/store"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

type viewsOutput struct {
	Statuses []struct {
		MessageID string `json:"message_id"`
		PostedAt  string `json:"posted_at"`
		Caption   string `json:"caption_snippet"`
		Count     int    `json:"unique_viewer_count"`
		Viewers   []struct {
			JID, Phone, Name string
			PushName         string `json:"push_name"`
			ReceiptType      string `json:"receipt_type"`
			ViewedAt         string `json:"viewed_at"`
		} `json:"viewers"`
	} `json:"statuses"`
}

func TestStatusViewsTool(t *testing.T) {
	e := newEnv(t, func(o *options) { o.sendDisabled = true })
	ctx := context.Background()
	now := e.clock.Now()
	for _, m := range []wa.Message{
		{ID: "recent", ChatJID: "status@broadcast", FromMe: true, Timestamp: now, Text: strings.Repeat("界", 141)},
		{ID: "old", ChatJID: "status@broadcast", FromMe: true, Timestamp: now.Add(-72 * time.Hour)},
		{ID: "empty", ChatJID: "status@broadcast", FromMe: true, Timestamp: now.Add(time.Second)},
	} {
		if err := e.store.SaveMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	e.fake.SetContacts([]wa.Contact{{JID: aliceJID, Name: "Saved", PushName: "Push"}})
	if err := e.store.SaveStatusViews(ctx, []wa.StatusView{
		{MessageID: "recent", ViewerJID: aliceJID, ReceiptType: "played", Timestamp: now},
		{MessageID: "recent", ViewerJID: aliceJID, ReceiptType: "read", Timestamp: now},
		{MessageID: "recent", ViewerJID: bobJID, ReceiptType: "read", Timestamp: now.Add(time.Second)},
	}); err != nil {
		t.Fatal(err)
	}
	got := callAs[viewsOutput](e, "status_views", map[string]any{})
	if len(got.Statuses) != 2 || got.Statuses[0].MessageID != "empty" || got.Statuses[0].Count != 0 || len(got.Statuses[0].Viewers) != 0 {
		t.Fatalf("got=%+v", got)
	}
	recent := got.Statuses[1]
	if recent.Count != 2 || len(recent.Viewers) != 2 || recent.Viewers[0].Name != "Saved" || recent.Viewers[0].PushName != "Push" || recent.Viewers[0].Phone != "33611111111" || recent.Viewers[0].ReceiptType != "read" || len([]rune(recent.Caption)) != 141 || !strings.HasSuffix(recent.Caption, "…") {
		t.Fatalf("recent=%+v", recent)
	}
	got = callAs[viewsOutput](e, "status_views", map[string]any{"since": now.Add(-96 * time.Hour).Format(time.RFC3339)})
	if len(got.Statuses) != 3 {
		t.Fatal(got)
	}
	got = callAs[viewsOutput](e, "status_views", map[string]any{"message_id": "old", "since": now.Add(time.Hour).Format(time.RFC3339)})
	if len(got.Statuses) != 1 || got.Statuses[0].MessageID != "old" {
		t.Fatal(got)
	}
	e.fail("status_views", map[string]any{"since": "invalid"}, "since")
	rows, err := e.store.ListStatusViews(ctx, store.StatusViewsQuery{MessageID: "recent"})
	if err != nil || len(rows) != 1 || len(rows[0].Viewers) != 3 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}
