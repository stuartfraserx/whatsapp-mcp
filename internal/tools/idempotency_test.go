package tools_test

import (
	"encoding/json"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
	"testing"
	"time"
)

func TestTextSendIdempotency(t *testing.T) {
	e := newEnv(t)
	args := map[string]any{"recipient": aliceJID, "text": "one"}
	first := callAs[sendResult](e, "send_message", args)
	second := callAs[sendResult](e, "send_message", args)
	if first.MessageID != second.MessageID || len(e.fake.Sent()) != 1 {
		t.Fatal("duplicate text sent twice")
	}
	e.clock.Advance(10*time.Minute + time.Second)
	third := callAs[sendResult](e, "send_message", args)
	if third.MessageID == first.MessageID || len(e.fake.Sent()) != 2 {
		t.Fatal("expiry failed")
	}
}
func TestTextSendClientKeyOverridesText(t *testing.T) {
	e := newEnv(t)
	args := map[string]any{"recipient": aliceJID, "text": "one", "idempotency_key": "key"}
	first := callAs[sendResult](e, "send_message", args)
	args["text"] = "changed"
	second := callAs[sendResult](e, "send_message", args)
	if first.MessageID != second.MessageID || len(e.fake.Sent()) != 1 {
		t.Fatal("client key failed")
	}
}
func TestMaybeSentToolResultRetainsID(t *testing.T) {
	e := newEnv(t)
	e.fake.SetSendError(&wa.MaybeSentError{MessageID: "generated-before-ack"})
	args := map[string]any{"recipient": aliceJID, "text": "one"}
	for i := 0; i < 2; i++ {
		result := e.call("send_message", args)
		raw, _ := json.Marshal(result.StructuredContent)
		var out sendResult
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if !result.IsError || out.MessageID != "generated-before-ack" || out.Status != "unknown_maybe_sent" {
			t.Fatalf("result=%s", raw)
		}
		e.fake.SetSendError(nil)
	}
	if len(e.fake.Sent()) != 0 {
		t.Fatal("maybe sent retry called client")
	}
}

func TestMutationRetriesDoNotSendAgain(t *testing.T) {
	for _, tool := range []string{"edit_message", "delete_message"} {
		t.Run(tool, func(t *testing.T) {
			e := newEnv(t)
			e.seedMessages(wa.Message{ChatJID: aliceJID, ID: "original", FromMe: true, Timestamp: epoch, Text: "before"})
			args := map[string]any{"chat": aliceJID, "message_id": "original"}
			if tool == "edit_message" {
				args["text"] = "after"
			}
			first := e.ok(tool, args)
			second := e.ok(tool, args)
			if len(e.fake.Sent()) != 1 {
				t.Fatal("mutation retry called WhatsApp twice")
			}
			a, _ := json.Marshal(first.StructuredContent)
			b, _ := json.Marshal(second.StructuredContent)
			if string(a) != string(b) {
				t.Fatal("mutation result changed")
			}
		})
	}
}
