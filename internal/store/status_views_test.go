package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/store"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

func TestStatusViews(t *testing.T) {
	s, _ := openStore(t)
	ctx := context.Background()
	for _, m := range []wa.Message{
		{ID: "recent", ChatJID: "status@broadcast", FromMe: true, Timestamp: t0, Text: "caption"},
		{ID: "old", ChatJID: "status@broadcast", FromMe: true, Timestamp: t0.Add(-72 * time.Hour)},
		{ID: "other", ChatJID: "status@broadcast", Timestamp: t0},
		{ID: "empty", ChatJID: "status@broadcast", FromMe: true, Timestamp: t0.Add(time.Hour)},
	} {
		save(t, s, m)
	}
	views := []wa.StatusView{
		{MessageID: "recent", ViewerJID: alice, ReceiptType: "read", Timestamp: t0},
		{MessageID: "recent", ViewerJID: alice, ReceiptType: "played", Timestamp: t0.Add(time.Second)},
		{MessageID: "recent", ViewerJID: bob, ReceiptType: "read", Timestamp: t0.Add(time.Second)},
		{MessageID: "orphan", ViewerJID: bob, ReceiptType: "read", Timestamp: t0},
		{MessageID: "", ViewerJID: bob}, {MessageID: "recent"},
	}
	if err := s.SaveStatusViews(ctx, views); err != nil {
		t.Fatal(err)
	}
	views[0].Timestamp = t0.Add(time.Hour)
	if err := s.SaveStatusViews(ctx, views); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListStatusViews(ctx, store.StatusViewsQuery{Since: t0.Add(-48 * time.Hour)})
	if err != nil || len(got) != 3 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if got[0].MessageID != "empty" || len(got[0].Viewers) != 0 || got[1].MessageID != "recent" || len(got[1].Viewers) != 3 || !got[1].Viewers[0].Timestamp.Equal(t0) || !got[2].PostedAt.IsZero() {
		t.Fatalf("got=%+v", got)
	}
	for _, id := range []string{"old", "orphan"} {
		got, err = s.ListStatusViews(ctx, store.StatusViewsQuery{MessageID: id, Since: t0.Add(time.Hour)})
		if err != nil || len(got) != 1 || got[0].MessageID != id {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	}
}
