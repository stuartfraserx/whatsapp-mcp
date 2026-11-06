package tools_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

func TestMessageMutationsSuccess(t *testing.T) {
	for _, tool := range []string{"edit_message", "delete_message"} {
		t.Run(tool, func(t *testing.T) {
			e := newEnv(t)
			m := wa.Message{ChatJID: aliceJID, ID: "original", SenderJID: selfJID, FromMe: true, Timestamp: epoch.Add(-time.Minute), Text: "before"}
			if tool == "delete_message" {
				m.Media = &wa.Media{Type: wa.MediaImage}
			}
			e.seedMessages(m)
			args := map[string]any{"chat": "+33611111111", "message_id": m.ID}
			status, operation := "revoked", "revoke"
			if tool == "edit_message" {
				args["text"] = "after 😀"
				status, operation = "edited", "edit"
			}
			out := callAs[sendResult](e, tool, args)
			if out.Status != status || out.MessageID != m.ID || out.ChatJID != aliceJID || out.Timestamp != epoch.Format(time.RFC3339) {
				t.Fatalf("output = %+v", out)
			}
			saved, err := e.store.GetMessage(e.ctx(), aliceJID, m.ID)
			if err != nil || !saved.Timestamp.Equal(m.Timestamp) {
				t.Fatalf("saved = %+v, err=%v", saved, err)
			}
			if tool == "edit_message" {
				if saved.Text != "after 😀" || !saved.Edited || !saved.EditedAt.Equal(epoch) {
					t.Fatalf("saved = %+v", saved)
				}
			} else if !saved.Revoked || !saved.RevokedAt.Equal(epoch) || saved.Text != "before" {
				t.Fatalf("saved = %+v", saved)
			}
			sent := e.fake.Sent()
			if len(sent) != 1 || sent[0].MessageID != m.ID || sent[0].Operation != operation || sent[0].ChatJID != aliceJID {
				t.Fatalf("sent = %+v", sent)
			}
			read := resultText(e.ok("list_messages", map[string]any{"chat_jid": aliceJID}))
			if !strings.Contains(read, fmt.Sprintf(`"%s":true`, status)) {
				t.Fatalf("read output = %s", read)
			}
		})
	}
}

func TestMessageMutationRefusals(t *testing.T) {
	for _, tool := range []string{"edit_message", "delete_message"} {
		window := 48 * time.Hour
		if tool == "edit_message" {
			window = 15 * time.Minute
		}
		type refusal struct {
			name, want string
			change     func(*wa.Message)
			opts       []func(*options)
			chat, text string
			missing    bool
		}
		tests := []refusal{
			{name: "not found", want: "not found", missing: true},
			{name: "not own", want: "only own", change: func(m *wa.Message) { m.FromMe = false }},
			{name: "too old", want: "window", change: func(m *wa.Message) { m.Timestamp = epoch.Add(-window - time.Nanosecond) }},
			{name: "revoked", want: "already revoked", change: func(m *wa.Message) { m.Revoked = true }},
			{name: "policy disabled", want: "disabled", opts: []func(*options){func(o *options) { o.policyOff = true }}},
			{name: "no policy", want: "not configured", opts: []func(*options){func(o *options) { o.noPolicy = true }}},
			{name: "not connected", want: "not connected", opts: []func(*options){withStatus(wa.Status{Paired: true, State: wa.StateDisconnected})}},
			{name: "not paired", want: "not paired", opts: []func(*options){withStatus(wa.Status{})}},
			{name: "allowlist", want: "allowlist", opts: []func(*options){func(o *options) { o.allowlist = []string{bobJID} }}},
			{name: "name", chat: "Alice Smith", want: "names are not accepted"},
			{name: "fuzzy name", chat: "Alice", want: "names are not accepted"},
		}
		if tool == "edit_message" {
			tests = append(tests,
				refusal{name: "media", want: "only text", change: func(m *wa.Message) { m.Media = &wa.Media{Type: wa.MediaImage} }},
				refusal{name: "blank", want: "blank", text: " "},
				refusal{name: "long", want: "4096", text: strings.Repeat("😀", 4097)},
			)
		}
		for _, tt := range tests {
			t.Run(tool+"/"+tt.name, func(t *testing.T) {
				e := newEnv(t, tt.opts...)
				seedAddressBook(e)
				m := wa.Message{ChatJID: aliceJID, ID: "m", SenderJID: selfJID, FromMe: true, Timestamp: epoch, Text: "before"}
				if tt.change != nil {
					tt.change(&m)
				}
				if !tt.missing {
					e.seedMessages(m)
				}
				chat := tt.chat
				if chat == "" {
					chat = aliceJID
				}
				args := map[string]any{"chat": chat, "message_id": "m"}
				if tool == "edit_message" {
					text := tt.text
					if text == "" {
						text = "after"
					}
					args["text"] = text
				}
				e.fail(tool, args, tt.want)
				e.assertNothingSent()
				if !tt.missing {
					saved, err := e.store.GetMessage(e.ctx(), aliceJID, "m")
					if err != nil || saved.Text != "before" {
						t.Fatalf("message changed: %+v %v", saved, err)
					}
				}
			})
		}
	}
}

func TestMutationRateLimitRefund(t *testing.T) {
	for _, tool := range []string{"edit_message", "delete_message"} {
		for _, definitelyNotSent := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/refund=%v", tool, definitelyNotSent), func(t *testing.T) {
				e := newEnv(t, withRate(time.Minute, 1000))
				e.seedMessages(wa.Message{ChatJID: aliceJID, ID: "m", FromMe: true, Timestamp: epoch, Text: "before"})
				args := map[string]any{"chat": aliceJID, "message_id": "m"}
				if tool == "edit_message" {
					args["text"] = "after"
				}
				failure := fmt.Errorf("ack timeout")
				if definitelyNotSent {
					failure = fmt.Errorf("%w: failed", wa.ErrNotSent)
				}
				e.fake.SetSendError(failure)
				e.fail(tool, args, "send failed")
				e.fake.SetSendError(nil)
				if definitelyNotSent {
					e.ok(tool, args)
				} else {
					e.fail(tool, args, "rate limit")
				}
			})
		}
	}
}

func TestMutationDescriptionsAndAnnotations(t *testing.T) {
	got := listTools(t, newEnv(t))
	for _, name := range []string{"edit_message", "delete_message"} {
		tool := got[name]
		for _, wording := range []string{"own", "user's real WhatsApp account", "visible to the other people", "user explicitly asked"} {
			if !strings.Contains(tool.Description, wording) {
				t.Errorf("%s missing %q", name, wording)
			}
		}
		if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint || tool.Annotations.ReadOnlyHint {
			t.Errorf("%s annotations=%+v", name, tool.Annotations)
		}
	}
}

func TestMutationWindowUsesOriginalTimestamp(t *testing.T) {
	e := newEnv(t)
	e.seedMessages(wa.Message{ChatJID: aliceJID, ID: "m", FromMe: true, Timestamp: epoch.Add(-15 * time.Minute), Text: "before"})
	args := map[string]any{"chat": aliceJID, "message_id": "m", "text": strings.Repeat("😀", 4096)}
	e.ok("edit_message", args)
	e.clock.Advance(time.Nanosecond)
	e.fail("edit_message", args, "15-minute WhatsApp edit window")
	if len(e.fake.Sent()) != 1 {
		t.Fatal("expired edit sent")
	}
}

func TestDeleteWindowBoundary(t *testing.T) {
	e := newEnv(t)
	e.seedMessages(wa.Message{ChatJID: aliceJID, ID: "m", FromMe: true, Timestamp: epoch.Add(-48 * time.Hour), Text: "before"})
	e.ok("delete_message", map[string]any{"chat": aliceJID, "message_id": "m"})
}
