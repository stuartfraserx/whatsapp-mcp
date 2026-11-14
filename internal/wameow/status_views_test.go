package wameow

import (
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestStatusViewReceipts(t *testing.T) {
	c := pairedTestClient(t)
	var got []wa.Event
	c.SetEventHandler(func(e wa.Event) { got = append(got, e) })
	viewer := types.NewJID("573001234567", types.DefaultUserServer)
	for _, tt := range []struct {
		kind         types.ReceiptType
		chat, sender types.JID
		want         bool
	}{
		{types.ReceiptTypeRead, types.StatusBroadcastJID, viewer, true},
		{types.ReceiptTypePlayed, types.StatusBroadcastJID, viewer, true},
		{types.ReceiptTypeReadSelf, types.StatusBroadcastJID, viewer, false},
		{"", types.StatusBroadcastJID, viewer, false},
		{types.ReceiptTypeRead, viewer, viewer, false},
		{types.ReceiptTypeRead, types.StatusBroadcastJID, c.ownJID(), false},
		{types.ReceiptTypePlayedSelf, types.StatusBroadcastJID, viewer, false},
		{types.ReceiptTypeSender, types.StatusBroadcastJID, viewer, false},
	} {
		got = nil
		c.handleEvent(&events.Receipt{MessageSource: types.MessageSource{Chat: tt.chat, Sender: tt.sender}, Type: tt.kind, MessageIDs: []types.MessageID{"one", "two", ""}, Timestamp: time.Unix(123, 0)})
		if !tt.want {
			if len(got) != 0 {
				t.Fatalf("unexpected event: %+v", got)
			}
			continue
		}
		if len(got) != 1 {
			t.Fatalf("events=%+v", got)
		}
		event, ok := got[0].(wa.StatusViewEvent)
		if !ok || len(event.Views) != 2 || event.Views[0].ViewerJID != viewer.String() || event.Views[0].MessageID != "one" || event.Views[1].MessageID != "two" || event.Views[0].ReceiptType != string(tt.kind) {
			t.Fatalf("event=%+v", got[0])
		}
	}
}

func TestOwnStatusMessage(t *testing.T) {
	c := pairedTestClient(t)
	var got []wa.Event
	c.SetEventHandler(func(e wa.Event) { got = append(got, e) })
	for _, own := range []bool{false, true} {
		got = nil
		c.handleEvent(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: types.StatusBroadcastJID, Sender: c.ownJID(), IsFromMe: own}, ID: "status", Timestamp: time.Unix(123, 0)}, Message: &waE2E.Message{Conversation: proto.String("caption")}})
		if !own {
			if len(got) != 0 {
				t.Fatal(got)
			}
			continue
		}
		if len(got) != 1 {
			t.Fatal(got)
		}
		m, ok := got[0].(wa.MessageEvent)
		if !ok || m.Message.Text != "caption" || !m.Message.FromMe || m.Message.ChatJID != "status@broadcast" {
			t.Fatalf("event=%+v", got[0])
		}
	}
}
