package tools_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

func TestPostStatusMediaAndCaption(t *testing.T) {
	for _, ext := range []string{"jpg", "png", "webp", "mp4"} {
		t.Run(ext, func(t *testing.T) {
			e := newEnv(t, withAllowlist(aliceJID))
			data := []byte("test image bytes")
			path := newOutboxFile(e, "story."+ext, data)
			out := callAs[sendResult](e, "post_status", map[string]any{"path": path, "caption": "A story 😀"})
			if out.Status != "sent" || out.ChatJID != "status@broadcast" || out.MessageID == "" || out.Timestamp == "" {
				t.Fatalf("output=%+v", out)
			}
			sent := e.fake.Sent()
			if len(sent) != 1 {
				t.Fatalf("sends=%d", len(sent))
			}
			r := sent[0]
			if r.Operation != "post_status" || r.ChatJID != "status@broadcast" || r.File == nil || !bytes.Equal(r.File.Data, data) || r.File.Caption != "A story 😀" || r.File.MimeType == "" {
				t.Fatalf("record=%+v", r)
			}
			wantMime := map[string]string{"jpg": "image/jpeg", "png": "image/png", "webp": "image/webp", "mp4": "video/mp4"}[ext]
			if r.File.MimeType != wantMime {
				t.Fatalf("mime=%s", r.File.MimeType)
			}
			saved, err := e.store.GetMessage(e.ctx(), "status@broadcast", out.MessageID)
			if err != nil || saved.Text != "A story 😀" {
				t.Fatalf("saved=%+v err=%v", saved, err)
			}
		})
	}
}

func TestPostStatusValidation(t *testing.T) {
	for _, tc := range []struct {
		name, filename, caption, want string
		data                          []byte
		outside                       bool
	}{
		{name: "empty path", want: "path is required"},
		{name: "outside", filename: "story.png", data: []byte("x"), outside: true, want: "inside the outbox or download"},
		{name: "empty", filename: "empty.png", want: "file is empty"},
		{name: "gif", filename: "story.gif", data: []byte("GIF89a"), want: "status requires a JPEG, PNG or WebP image or a video; GIF, audio, documents and stickers are not supported"},
		{name: "pdf", filename: "doc.pdf", data: []byte("x"), want: "status requires a JPEG, PNG or WebP image or a video; GIF, audio, documents and stickers are not supported"},
		{name: "audio", filename: "sound.mp3", data: []byte("x"), want: "status requires a JPEG, PNG or WebP image or a video; GIF, audio, documents and stickers are not supported"},
		{name: "sticker", filename: "sticker.tgs", data: []byte("x"), want: "status requires a JPEG, PNG or WebP image or a video; GIF, audio, documents and stickers are not supported"},
		{name: "caption", filename: "story.png", data: []byte("x"), caption: strings.Repeat("😀", 4097), want: "caption must be at most"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			path := " "
			if tc.filename != "" {
				dir := e.outbox
				if tc.outside {
					dir = t.TempDir()
				}
				path = e.writeFile(filepath.Join(dir, tc.filename), tc.data)
			}
			e.fail("post_status", map[string]any{"path": path, "caption": tc.caption}, tc.want)
			e.assertNothingSent()
		})
	}
	e := newEnv(t)
	e.fail("post_status", map[string]any{}, "path")
	path := filepath.Join(e.outbox, "huge.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate((64 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	e.fail("post_status", map[string]any{"path": path}, "64 MiB")
	e.assertNothingSent()
}

func TestPostStatusConnectionAndPolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*options)
		want string
	}{
		{"unpaired", withStatus(wa.Status{State: wa.StateNeedsQR}), "not paired"},
		{"disconnected", withStatus(wa.Status{Paired: true, State: wa.StateDisconnected}), "not connected"},
		{"disabled policy", withPolicyOff, "sending is disabled"},
		{"missing policy", withNoPolicy, "send policy is not configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.mod)
			path := newOutboxFile(e, "story.png", []byte("x"))
			e.fail("post_status", map[string]any{"path": path}, tc.want)
			e.assertNothingSent()
		})
	}
}

func TestPostStatusRetryWindowAndSharedRateLimit(t *testing.T) {
	e := newEnv(t, withRate(time.Second, 100))
	path := newOutboxFile(e, "story.png", []byte("x"))
	args := map[string]any{"path": path, "idempotency_key": "story-key"}
	first := callAs[sendResult](e, "post_status", args)
	args["caption"] = "changed on retry"
	second := callAs[sendResult](e, "post_status", args)
	if first.MessageID != second.MessageID || len(e.fake.Sent()) != 1 {
		t.Fatal("posted twice")
	}
	e.fail("post_status", map[string]any{"path": path, "idempotency_key": "another"}, "rate limit")
	e.fail("send_message", map[string]any{"recipient": aliceJID, "text": "hello"}, "rate limit")
	e.clock.Advance(10*time.Minute + time.Second)
	third := callAs[sendResult](e, "post_status", args)
	if third.MessageID == first.MessageID || len(e.fake.Sent()) != 2 {
		t.Fatal("retry window did not expire")
	}
}

func TestPostStatusMaybeSentDoesNotRetry(t *testing.T) {
	e := newEnv(t)
	path := newOutboxFile(e, "story.png", []byte("x"))
	e.fake.SetSendError(&wa.MaybeSentError{MessageID: "status-id"})
	for i := 0; i < 2; i++ {
		res := e.call("post_status", map[string]any{"path": path})
		raw, _ := json.Marshal(res.StructuredContent)
		var out sendResult
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if !res.IsError || out.Status != "unknown_maybe_sent" || out.MessageID != "status-id" || out.ChatJID != "status@broadcast" {
			t.Fatalf("result=%s", raw)
		}
		e.fake.SetSendError(nil)
	}
	e.assertNothingSent()
}

func TestPostStatusDownloadDirectory(t *testing.T) {
	e := newEnv(t)
	path := e.writeFile(filepath.Join(e.downloads, "story.png"), []byte("x"))
	e.ok("post_status", map[string]any{"path": path})
}
