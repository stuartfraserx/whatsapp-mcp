package wameow

import (
	"bytes"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

var (
	pnAlice  = types.NewJID("33611111111", types.DefaultUserServer)
	pnBob    = types.NewJID("33622222222", types.DefaultUserServer)
	lidAlice = types.NewJID("98765432101234", types.HiddenUserServer)
	group    = types.NewJID("120363000000000001", types.GroupServer)
)

func withDevice(j types.JID, device uint16) types.JID {
	j.Device = device
	return j
}

func testUpload() uploaded {
	return uploaded{
		URL:           "https://mmg.whatsapp.net/v/t62/file",
		DirectPath:    "/v/t62/file",
		MediaKey:      []byte{1, 2, 3},
		FileEncSHA256: []byte{4, 5},
		FileSHA256:    []byte{6, 7, 8},
		FileLength:    4242,
	}
}

func TestSendKindFollowsOutgoingMediaType(t *testing.T) {
	tests := []struct {
		mime string
		want whatsmeow.MediaType
	}{
		{"image/jpeg", whatsmeow.MediaImage},
		{"IMAGE/PNG; charset=binary", whatsmeow.MediaImage},
		{"image/gif", whatsmeow.MediaDocument},
		{"video/mp4", whatsmeow.MediaVideo},
		{"audio/ogg; codecs=opus", whatsmeow.MediaAudio},
		{"application/pdf", whatsmeow.MediaDocument},
		{"", whatsmeow.MediaDocument},
	}
	for _, tt := range tests {
		t.Run(tt.mime, func(t *testing.T) {
			if got := whatsmeowMediaType(wa.OutgoingMediaType(tt.mime)); got != tt.want {
				t.Fatalf("upload type for %q = %q, want %q", tt.mime, got, tt.want)
			}
		})
	}
}

func TestWhatsmeowMediaType(t *testing.T) {
	tests := []struct {
		kind wa.MediaType
		want whatsmeow.MediaType
	}{
		{wa.MediaImage, whatsmeow.MediaImage},
		{wa.MediaSticker, whatsmeow.MediaImage},
		{wa.MediaVideo, whatsmeow.MediaVideo},
		{wa.MediaAudio, whatsmeow.MediaAudio},
		{wa.MediaDocument, whatsmeow.MediaDocument},
		{"", whatsmeow.MediaDocument},
	}
	for _, tt := range tests {
		if got := whatsmeowMediaType(tt.kind); got != tt.want {
			t.Errorf("whatsmeowMediaType(%q) = %q, want %q", tt.kind, got, tt.want)
		}
	}
}

func TestBuildOutgoingMediaImageAndVideo(t *testing.T) {
	up := testUpload()
	file := wa.OutgoingFile{FileName: "pic.png", MimeType: "image/png", Caption: "look"}

	img := buildOutgoingMedia(wa.MediaImage, up, file).GetImageMessage()
	if img == nil {
		t.Fatal("image kind must produce an ImageMessage")
	}
	if img.GetURL() != up.URL || img.GetDirectPath() != up.DirectPath || !bytes.Equal(img.GetMediaKey(), up.MediaKey) ||
		!bytes.Equal(img.GetFileEncSHA256(), up.FileEncSHA256) || !bytes.Equal(img.GetFileSHA256(), up.FileSHA256) ||
		img.GetFileLength() != up.FileLength || img.GetMimetype() != "image/png" || img.GetCaption() != "look" {
		t.Fatalf("image message = %v", img)
	}

	vid := buildOutgoingMedia(wa.MediaVideo, up, wa.OutgoingFile{MimeType: "video/mp4", Caption: "clip"}).GetVideoMessage()
	if vid == nil {
		t.Fatal("video kind must produce a VideoMessage")
	}
	if vid.GetURL() != up.URL || vid.GetDirectPath() != up.DirectPath || vid.GetFileLength() != up.FileLength ||
		vid.GetMimetype() != "video/mp4" || vid.GetCaption() != "clip" || !bytes.Equal(vid.GetMediaKey(), up.MediaKey) {
		t.Fatalf("video message = %v", vid)
	}
}

func TestBuildOutgoingMediaAudioDropsCaption(t *testing.T) {
	up := testUpload()

	msg := buildOutgoingMedia(wa.MediaAudio, up, wa.OutgoingFile{MimeType: "audio/ogg", Caption: "ignored"})

	audio := msg.GetAudioMessage()
	if audio == nil || msg.GetImageMessage() != nil || msg.GetDocumentMessage() != nil {
		t.Fatalf("audio kind must produce only an AudioMessage: %v", msg)
	}
	if audio.GetMimetype() != "audio/ogg" || audio.GetFileLength() != up.FileLength || audio.GetDirectPath() != up.DirectPath {
		t.Fatalf("audio message = %v", audio)
	}
	if text, media := extractContent(msg); text != "" || media == nil || media.Type != wa.MediaAudio {
		t.Fatalf("round trip text=%q media=%+v", text, media)
	}
}

func TestBuildOutgoingMediaDocument(t *testing.T) {
	up := testUpload()

	for _, kind := range []wa.MediaType{wa.MediaDocument, wa.MediaSticker, ""} {
		msg := buildOutgoingMedia(kind, up, wa.OutgoingFile{FileName: "report.pdf", MimeType: "application/pdf", Caption: "Q3"})

		doc := msg.GetDocumentMessage()
		if doc == nil {
			t.Fatalf("kind %q must fall back to a DocumentMessage", kind)
		}
		if doc.GetFileName() != "report.pdf" || doc.GetTitle() != "report.pdf" || doc.GetCaption() != "Q3" ||
			doc.GetMimetype() != "application/pdf" || doc.GetFileLength() != up.FileLength || doc.GetURL() != up.URL ||
			!bytes.Equal(doc.GetFileSHA256(), up.FileSHA256) || !bytes.Equal(doc.GetFileEncSHA256(), up.FileEncSHA256) {
			t.Fatalf("document message for kind %q = %v", kind, doc)
		}
	}
}

func TestBuildOutgoingMediaOptionalFields(t *testing.T) {
	up := testUpload()

	tests := []struct {
		name string
		kind wa.MediaType
		file wa.OutgoingFile
	}{
		{"image without caption", wa.MediaImage, wa.OutgoingFile{MimeType: "image/png"}},
		{"video without caption", wa.MediaVideo, wa.OutgoingFile{MimeType: "video/mp4"}},
		{"document without caption or name", wa.MediaDocument, wa.OutgoingFile{MimeType: "application/pdf"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := buildOutgoingMedia(tt.kind, up, tt.file)

			switch {
			case msg.GetImageMessage() != nil:
				if msg.GetImageMessage().Caption != nil {
					t.Fatalf("empty caption must stay unset, got %q", msg.GetImageMessage().GetCaption())
				}
			case msg.GetVideoMessage() != nil:
				if msg.GetVideoMessage().Caption != nil {
					t.Fatalf("empty caption must stay unset")
				}
			case msg.GetDocumentMessage() != nil:
				d := msg.GetDocumentMessage()
				if d.Caption != nil || d.FileName != nil || d.Title != nil {
					t.Fatalf("empty optional fields must stay unset: %v", d)
				}
			default:
				t.Fatalf("no media message built: %v", msg)
			}
		})
	}
}

func TestBuildOutgoingMediaDefaultsMissingMIMEType(t *testing.T) {
	up := testUpload()

	for _, kind := range []wa.MediaType{wa.MediaImage, wa.MediaVideo, wa.MediaAudio, wa.MediaDocument} {
		msg := buildOutgoingMedia(kind, up, wa.OutgoingFile{})
		var got string
		switch {
		case msg.GetImageMessage() != nil:
			got = msg.GetImageMessage().GetMimetype()
		case msg.GetVideoMessage() != nil:
			got = msg.GetVideoMessage().GetMimetype()
		case msg.GetAudioMessage() != nil:
			got = msg.GetAudioMessage().GetMimetype()
		default:
			got = msg.GetDocumentMessage().GetMimetype()
		}
		if got != "application/octet-stream" {
			t.Errorf("kind %q: mimetype = %q, want application/octet-stream", kind, got)
		}
	}
}

func mediaFields() (path string, key, sha, encSHA []byte, length uint64) {
	return "/v/t62.7118-24/abc", []byte{9, 8, 7}, []byte{1, 1}, []byte{2, 2}, 1234
}

func assertMedia(t *testing.T, got *wa.Media, want wa.Media) {
	t.Helper()
	if got == nil {
		t.Fatal("media is nil")
	}
	if got.Type != want.Type || got.MimeType != want.MimeType || got.FileName != want.FileName ||
		got.DirectPath != want.DirectPath || got.URL != want.URL || got.FileLength != want.FileLength ||
		!bytes.Equal(got.MediaKey, want.MediaKey) || !bytes.Equal(got.FileSHA256, want.FileSHA256) ||
		!bytes.Equal(got.FileEncSHA256, want.FileEncSHA256) {
		t.Fatalf("media = %+v, want %+v", *got, want)
	}
}

func TestExtractContentText(t *testing.T) {
	tests := []struct {
		name string
		msg  *waE2E.Message
		want string
	}{
		{"conversation", &waE2E.Message{Conversation: proto.String("hello")}, "hello"},
		{"extended text", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("a link https://example.com")}}, "a link https://example.com"},
		{"extended text wins over conversation", &waE2E.Message{
			Conversation:        proto.String("old"),
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("new")},
		}, "new"},
		{"unicode and newlines kept", &waE2E.Message{Conversation: proto.String("héllo 😀\nline2")}, "héllo 😀\nline2"},
		{"empty conversation", &waE2E.Message{Conversation: proto.String("")}, ""},
		{"empty message", &waE2E.Message{}, ""},
		{"nil message", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, media := extractContent(tt.msg)
			if text != tt.want || media != nil {
				t.Fatalf("extractContent = (%q, %v), want (%q, nil)", text, media, tt.want)
			}
		})
	}
}

func TestExtractContentMediaKinds(t *testing.T) {
	path, key, sha, encSHA, length := mediaFields()
	tests := []struct {
		name     string
		msg      *waE2E.Message
		wantText string
		want     wa.Media
	}{
		{
			name: "image with caption",
			msg: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
				Caption: proto.String("sunset"), Mimetype: proto.String("image/jpeg"), DirectPath: proto.String(path),
				URL: proto.String("https://mmg/x"), MediaKey: key, FileSHA256: sha, FileEncSHA256: encSHA, FileLength: proto.Uint64(length),
			}},
			wantText: "sunset",
			want: wa.Media{Type: wa.MediaImage, MimeType: "image/jpeg", DirectPath: path, URL: "https://mmg/x",
				MediaKey: key, FileSHA256: sha, FileEncSHA256: encSHA, FileLength: length},
		},
		{
			name: "image without caption",
			msg: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
				Mimetype: proto.String("image/png"), DirectPath: proto.String(path), MediaKey: key, FileLength: proto.Uint64(length),
			}},
			want: wa.Media{Type: wa.MediaImage, MimeType: "image/png", DirectPath: path, MediaKey: key, FileLength: length},
		},
		{
			name: "video with caption",
			msg: &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
				Caption: proto.String("clip"), Mimetype: proto.String("video/mp4"), DirectPath: proto.String(path),
				URL: proto.String("https://mmg/v"), MediaKey: key, FileSHA256: sha, FileEncSHA256: encSHA, FileLength: proto.Uint64(length),
			}},
			wantText: "clip",
			want: wa.Media{Type: wa.MediaVideo, MimeType: "video/mp4", DirectPath: path, URL: "https://mmg/v",
				MediaKey: key, FileSHA256: sha, FileEncSHA256: encSHA, FileLength: length},
		},
		{
			name: "audio voice note has no text",
			msg: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
				Mimetype: proto.String("audio/ogg; codecs=opus"), DirectPath: proto.String(path), MediaKey: key,
				FileSHA256: sha, FileEncSHA256: encSHA, FileLength: proto.Uint64(length),
			}},
			want: wa.Media{Type: wa.MediaAudio, MimeType: "audio/ogg; codecs=opus", DirectPath: path,
				MediaKey: key, FileSHA256: sha, FileEncSHA256: encSHA, FileLength: length},
		},
		{
			name: "document with name and caption",
			msg: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
				FileName: proto.String("report.pdf"), Title: proto.String("ignored title"), Caption: proto.String("Q3 report"),
				Mimetype: proto.String("application/pdf"), DirectPath: proto.String(path), URL: proto.String("https://mmg/d"),
				MediaKey: key, FileSHA256: sha, FileEncSHA256: encSHA, FileLength: proto.Uint64(length),
			}},
			wantText: "Q3 report",
			want: wa.Media{Type: wa.MediaDocument, MimeType: "application/pdf", FileName: "report.pdf", DirectPath: path,
				URL: "https://mmg/d", MediaKey: key, FileSHA256: sha, FileEncSHA256: encSHA, FileLength: length},
		},
		{
			name: "document falls back to title for the file name",
			msg: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
				Title: proto.String("Budget"), Mimetype: proto.String("application/vnd.ms-excel"), DirectPath: proto.String(path), MediaKey: key,
			}},
			want: wa.Media{Type: wa.MediaDocument, MimeType: "application/vnd.ms-excel", FileName: "Budget", DirectPath: path, MediaKey: key},
		},
		{
			name: "sticker has no text",
			msg: &waE2E.Message{StickerMessage: &waE2E.StickerMessage{
				Mimetype: proto.String("image/webp"), DirectPath: proto.String(path), MediaKey: key,
				FileSHA256: sha, FileEncSHA256: encSHA, FileLength: proto.Uint64(length),
			}},
			want: wa.Media{Type: wa.MediaSticker, MimeType: "image/webp", DirectPath: path,
				MediaKey: key, FileSHA256: sha, FileEncSHA256: encSHA, FileLength: length},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, media := extractContent(tt.msg)

			if text != tt.wantText {
				t.Fatalf("text = %q, want %q", text, tt.wantText)
			}
			assertMedia(t, media, tt.want)
		})
	}
}

func TestExtractContentIgnoresNonContentMessages(t *testing.T) {
	tests := []struct {
		name string
		msg  *waE2E.Message
	}{
		{"reaction", &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}}},
		{"revoke protocol message", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("X")},
		}}},
		{"view once image stays unread", &waE2E.Message{ViewOnceMessage: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("secret"), DirectPath: proto.String("/p")}},
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if text, media := extractContent(tt.msg); text != "" || media != nil {
				t.Fatalf("extractContent = (%q, %+v), want nothing", text, media)
			}
		})
	}
}

func TestExtractContentUnwrapsDeliveryWrappers(t *testing.T) {
	inner := func() *waE2E.Message { return &waE2E.Message{Conversation: proto.String("wrapped")} }
	tests := []struct {
		name string
		msg  *waE2E.Message
	}{
		{"device sent", &waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{Message: inner()}}},
		{"ephemeral", &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: inner()}}},
		{"document with caption", &waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{Message: inner()}}},
		{"device sent around ephemeral", &waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{
			Message: &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: inner()}},
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if text, _ := extractContent(tt.msg); text != "wrapped" {
				t.Fatalf("text = %q, want %q", text, "wrapped")
			}
		})
	}
}

func TestExtractContentUnwrapsEphemeralDocumentWithCaption(t *testing.T) {
	path, key, _, _, length := mediaFields()
	msg := &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
		DocumentWithCaptionMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{
			DocumentMessage: &waE2E.DocumentMessage{
				FileName: proto.String("a.pdf"), Caption: proto.String("cap"), Mimetype: proto.String("application/pdf"),
				DirectPath: proto.String(path), MediaKey: key, FileLength: proto.Uint64(length),
			},
		}},
	}}}

	text, media := extractContent(msg)

	if text != "cap" {
		t.Fatalf("text = %q", text)
	}
	assertMedia(t, media, wa.Media{Type: wa.MediaDocument, MimeType: "application/pdf", FileName: "a.pdf", DirectPath: path, MediaKey: key, FileLength: length})
}

func TestUnwrapIsBoundedAgainstDeeplyNestedWrappers(t *testing.T) {
	nest := func(depth int) *waE2E.Message {
		msg := &waE2E.Message{Conversation: proto.String("core")}
		for range depth {
			msg = &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: msg}}
		}
		return msg
	}

	if text, _ := extractContent(nest(maxUnwrapDepth)); text != "core" {
		t.Fatalf("depth %d must still unwrap, got %q", maxUnwrapDepth, text)
	}
	if text, _ := extractContent(nest(maxUnwrapDepth + 1)); text != "" {
		t.Fatalf("depth %d must stop unwrapping, got %q", maxUnwrapDepth+1, text)
	}
	if text, media := extractContent(nest(10_000)); text != "" || media != nil {
		t.Fatalf("pathological nesting must yield nothing, got (%q, %v)", text, media)
	}
}

func editProto(targetID string, content *waE2E.Message) *waE2E.Message {
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		Key:           &waCommon.MessageKey{ID: proto.String(targetID)},
		EditedMessage: content,
	}}
}

func TestConvertEdit(t *testing.T) {
	info := types.MessageInfo{ID: "EDIT-EVENT-ID", Timestamp: time.Date(2025, 4, 5, 6, 7, 8, 0, time.UTC)}
	edited := &waE2E.Message{Conversation: proto.String("fixed typo")}

	t.Run("protocol edit targets the original message", func(t *testing.T) {
		got, isEdit, ok := convertEdit(info, editProto("ORIG", edited), false, pnAlice, pnBob)
		want := wa.MessageEditEvent{ChatJID: pnAlice.String(), ID: "ORIG", SenderJID: pnBob.String(), Text: "fixed typo"}
		if !isEdit || !ok || got != want {
			t.Fatalf("got (%+v, %v, %v), want %+v", got, isEdit, ok, want)
		}
	})
	t.Run("edit of a caption yields the new caption", func(t *testing.T) {
		img := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("new caption")}}
		got, isEdit, ok := convertEdit(info, editProto("ORIG", img), false, pnAlice, pnAlice)
		if !isEdit || !ok || got.Text != "new caption" || got.ID != "ORIG" {
			t.Fatalf("got (%+v, %v, %v)", got, isEdit, ok)
		}
	})
	t.Run("edit inside delivery wrappers is recognised", func(t *testing.T) {
		wrapped := &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: editProto("ORIG", edited)}}
		got, isEdit, ok := convertEdit(info, wrapped, false, pnAlice, pnAlice)
		if !isEdit || !ok || got.ID != "ORIG" {
			t.Fatalf("got (%+v, %v, %v)", got, isEdit, ok)
		}
	})
	t.Run("edit wrapper without protocol message uses the bot edit target", func(t *testing.T) {
		withTarget := info
		withTarget.MsgBotInfo.EditTargetID = "BOT-ORIG"
		got, isEdit, ok := convertEdit(withTarget, edited, true, pnAlice, pnAlice)
		if !isEdit || !ok || got.ID != "BOT-ORIG" || got.Text != "fixed typo" {
			t.Fatalf("got (%+v, %v, %v)", got, isEdit, ok)
		}
	})
	t.Run("edit wrapper without any target is dropped, not stored", func(t *testing.T) {
		_, isEdit, ok := convertEdit(info, edited, true, pnAlice, pnAlice)
		if !isEdit || ok {
			t.Fatalf("isEdit=%v ok=%v, want true/false", isEdit, ok)
		}
	})
	t.Run("protocol edit without a target id is dropped", func(t *testing.T) {
		_, isEdit, ok := convertEdit(info, editProto("", edited), false, pnAlice, pnAlice)
		if !isEdit || ok {
			t.Fatalf("isEdit=%v ok=%v, want true/false", isEdit, ok)
		}
	})
	t.Run("edit without text is dropped", func(t *testing.T) {
		_, isEdit, ok := convertEdit(info, editProto("ORIG", &waE2E.Message{}), false, pnAlice, pnAlice)
		if !isEdit || ok {
			t.Fatalf("isEdit=%v ok=%v, want true/false", isEdit, ok)
		}
	})
	t.Run("plain messages are not edits", func(t *testing.T) {
		_, isEdit, ok := convertEdit(info, &waE2E.Message{Conversation: proto.String("plain")}, false, pnAlice, pnAlice)
		if isEdit || ok {
			t.Fatalf("isEdit=%v ok=%v, want false/false", isEdit, ok)
		}
	})
	t.Run("other protocol messages are not edits", func(t *testing.T) {
		msg := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("TARGET")},
		}}
		if _, isEdit, ok := convertEdit(info, msg, false, pnAlice, pnAlice); isEdit || ok {
			t.Fatalf("isEdit=%v ok=%v, want false/false", isEdit, ok)
		}
	})
	t.Run("nil message is not an edit", func(t *testing.T) {
		if _, isEdit, ok := convertEdit(info, nil, false, pnAlice, pnAlice); isEdit || ok {
			t.Fatalf("isEdit=%v ok=%v, want false/false", isEdit, ok)
		}
	})
}

func TestDMChatNames(t *testing.T) {
	contacts := []wa.Contact{
		{JID: pnAlice.String(), Name: "Alice Full", PushName: "ally", PN: pnAlice.String(), LID: lidAlice.String()},
		{JID: pnBob.String(), Name: "Bob", PushName: "bobby", PN: pnBob.String()},
		{JID: "33633333333@s.whatsapp.net", PushName: "Carol", PN: "33633333333@s.whatsapp.net"},
		{JID: "33655555555@s.whatsapp.net", BusinessName: "ACME", PushName: "acme-bot", PN: "33655555555@s.whatsapp.net"},
		{JID: "11111111111111@lid", PushName: "Only Lid", LID: "11111111111111@lid"},
		{JID: "33644444444@s.whatsapp.net", PN: "33644444444@s.whatsapp.net"},
	}

	got := dmChatNames(contacts)

	names := map[string]string{}
	for _, c := range got {
		if c.IsGroup {
			t.Errorf("DM chat flagged as group: %+v", c)
		}
		names[c.JID] = c.Name
	}
	if !slices.IsSortedFunc(got, func(a, b wa.Chat) int { return strings.Compare(a.JID, b.JID) }) {
		t.Fatalf("chats = %+v, want sorted chats", got)
	}
	want := map[string]string{
		pnAlice.String():             "Alice Full",
		lidAlice.String():            "Alice Full",
		pnBob.String():               "Bob",
		"33633333333@s.whatsapp.net": "Carol",
		"33655555555@s.whatsapp.net": "ACME",
		"11111111111111@lid":         "Only Lid",
	}
	if !maps.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
}

func TestConvertMessage(t *testing.T) {
	loc := time.FixedZone("CEST", 2*3600)
	ts := time.Date(2025, 6, 7, 14, 30, 0, 0, loc)
	path, key, sha, encSHA, length := mediaFields()

	tests := []struct {
		name       string
		info       types.MessageInfo
		msg        *waE2E.Message
		chat       types.JID
		sender     types.JID
		wantOK     bool
		wantChat   string
		wantSender string
		wantName   string
		wantFromMe bool
		wantText   string
		wantMedia  *wa.Media
	}{
		{
			name:       "incoming dm text keeps push name",
			info:       types.MessageInfo{ID: "M1", Timestamp: ts, PushName: "Alice P"},
			msg:        &waE2E.Message{Conversation: proto.String("hi")},
			chat:       pnAlice,
			sender:     pnAlice,
			wantOK:     true,
			wantChat:   "33611111111@s.whatsapp.net",
			wantSender: "33611111111@s.whatsapp.net",
			wantName:   "Alice P",
			wantText:   "hi",
		},
		{
			name: "outgoing message drops push name",
			info: types.MessageInfo{
				ID: "M2", Timestamp: ts, PushName: "My Own Name",
				MessageSource: types.MessageSource{IsFromMe: true},
			},
			msg:        &waE2E.Message{Conversation: proto.String("yo")},
			chat:       pnBob,
			sender:     pnAlice,
			wantOK:     true,
			wantChat:   "33622222222@s.whatsapp.net",
			wantSender: "33611111111@s.whatsapp.net",
			wantName:   "",
			wantFromMe: true,
			wantText:   "yo",
		},
		{
			name:       "group message uses group chat and member sender",
			info:       types.MessageInfo{ID: "M3", Timestamp: ts, PushName: "Bob"},
			msg:        &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("team")}},
			chat:       group,
			sender:     pnBob,
			wantOK:     true,
			wantChat:   "120363000000000001@g.us",
			wantSender: "33622222222@s.whatsapp.net",
			wantName:   "Bob",
			wantText:   "team",
		},
		{
			name: "image with caption carries media",
			info: types.MessageInfo{ID: "M4", Timestamp: ts},
			msg: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
				Caption: proto.String("pic"), Mimetype: proto.String("image/jpeg"), DirectPath: proto.String(path),
				MediaKey: key, FileSHA256: sha, FileEncSHA256: encSHA, FileLength: proto.Uint64(length),
			}},
			chat:       pnAlice,
			sender:     pnAlice,
			wantOK:     true,
			wantChat:   "33611111111@s.whatsapp.net",
			wantSender: "33611111111@s.whatsapp.net",
			wantText:   "pic",
			wantMedia: &wa.Media{Type: wa.MediaImage, MimeType: "image/jpeg", DirectPath: path,
				MediaKey: key, FileSHA256: sha, FileEncSHA256: encSHA, FileLength: length},
		},
		{
			name: "audio without text is kept because it has media",
			info: types.MessageInfo{ID: "M5", Timestamp: ts},
			msg: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
				Mimetype: proto.String("audio/ogg"), DirectPath: proto.String(path), MediaKey: key,
			}},
			chat:       pnAlice,
			sender:     pnAlice,
			wantOK:     true,
			wantChat:   "33611111111@s.whatsapp.net",
			wantSender: "33611111111@s.whatsapp.net",
			wantMedia:  &wa.Media{Type: wa.MediaAudio, MimeType: "audio/ogg", DirectPath: path, MediaKey: key},
		},
		{name: "empty message is dropped", info: types.MessageInfo{ID: "M6", Timestamp: ts}, msg: &waE2E.Message{}, chat: pnAlice, sender: pnAlice},
		{
			name: "reaction is dropped",
			info: types.MessageInfo{ID: "M7", Timestamp: ts},
			msg:  &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("❤")}},
			chat: pnAlice, sender: pnAlice,
		},
		{name: "nil message is dropped", info: types.MessageInfo{ID: "M8", Timestamp: ts}, msg: nil, chat: pnAlice, sender: pnAlice},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := convertMessage(tt.info, tt.msg, tt.chat, tt.sender)

			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (message %+v)", ok, tt.wantOK, got)
			}
			if !ok {
				if got.ID != "" || got.Text != "" || got.Media != nil {
					t.Fatalf("dropped message must be zero, got %+v", got)
				}
				return
			}
			if got.ID != tt.info.ID || got.ChatJID != tt.wantChat || got.SenderJID != tt.wantSender ||
				got.SenderName != tt.wantName || got.FromMe != tt.wantFromMe || got.Text != tt.wantText {
				t.Fatalf("message = %+v", got)
			}
			if !got.Timestamp.Equal(ts) || got.Timestamp.Location() != time.UTC {
				t.Fatalf("timestamp = %v (%v), want %v normalised to UTC", got.Timestamp, got.Timestamp.Location(), ts.UTC())
			}
			if tt.wantMedia == nil {
				if got.Media != nil {
					t.Fatalf("unexpected media %+v", got.Media)
				}
				return
			}
			assertMedia(t, got.Media, *tt.wantMedia)
		})
	}
}

func TestPreferPN(t *testing.T) {
	lookupTo := func(pn types.JID) func(types.JID) (types.JID, bool) {
		return func(types.JID) (types.JID, bool) { return pn, true }
	}
	noLookup := func(types.JID) (types.JID, bool) { return types.EmptyJID, false }

	tests := []struct {
		name   string
		jid    types.JID
		alt    types.JID
		lookup func(types.JID) (types.JID, bool)
		want   types.JID
	}{
		{"phone number jid is returned as is", pnAlice, types.EmptyJID, noLookup, pnAlice},
		{"phone number jid ignores alt", pnAlice, pnBob, lookupTo(pnBob), pnAlice},
		{"device suffix stripped from phone number", withDevice(pnAlice, 7), types.EmptyJID, nil, pnAlice},
		{"group jid untouched", group, pnBob, lookupTo(pnBob), group},
		{"lid replaced by alt phone number", lidAlice, pnAlice, noLookup, pnAlice},
		{"alt phone number loses device suffix", lidAlice, withDevice(pnAlice, 3), nil, pnAlice},
		{"lid with device uses alt", withDevice(lidAlice, 12), pnAlice, nil, pnAlice},
		{"lid resolved through lookup", lidAlice, types.EmptyJID, lookupTo(pnAlice), pnAlice},
		{"lookup result loses device suffix", lidAlice, types.EmptyJID, lookupTo(withDevice(pnAlice, 9)), pnAlice},
		{"alt that is itself a lid falls back to lookup", lidAlice, types.NewJID("555", types.HiddenUserServer), lookupTo(pnAlice), pnAlice},
		{"alt phone number without user falls back to lookup", lidAlice, types.NewJID("", types.DefaultUserServer), lookupTo(pnAlice), pnAlice},
		{"unknown lid stays a lid without device", withDevice(lidAlice, 5), types.EmptyJID, noLookup, lidAlice},
		{"nil lookup keeps the lid", lidAlice, types.EmptyJID, nil, lidAlice},
		{"lookup returning a non phone jid is rejected", lidAlice, types.EmptyJID, lookupTo(types.NewJID("77", types.HiddenUserServer)), lidAlice},
		{"lookup returning an empty user is rejected", lidAlice, types.EmptyJID, lookupTo(types.NewJID("", types.DefaultUserServer)), lidAlice},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := preferPN(tt.jid, tt.alt, tt.lookup); got != tt.want {
				t.Fatalf("preferPN = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPreferPNOnlyConsultsLookupWhenAltIsUnusable(t *testing.T) {
	calls := 0
	lookup := func(types.JID) (types.JID, bool) {
		calls++
		return pnBob, true
	}

	if got := preferPN(lidAlice, pnAlice, lookup); got != pnAlice || calls != 0 {
		t.Fatalf("alt available: got %v after %d lookups", got, calls)
	}
	if got := preferPN(pnAlice, types.EmptyJID, lookup); got != pnAlice || calls != 0 {
		t.Fatalf("phone number input: got %v after %d lookups", got, calls)
	}
	if got := preferPN(lidAlice, types.EmptyJID, lookup); got != pnBob || calls != 1 {
		t.Fatalf("no alt: got %v after %d lookups", got, calls)
	}
}

func TestPreferPNPassesTheNormalisedLIDToLookup(t *testing.T) {
	var seen types.JID
	lookup := func(j types.JID) (types.JID, bool) {
		seen = j
		return pnAlice, true
	}

	preferPN(withDevice(lidAlice, 4), types.EmptyJID, lookup)

	if seen != lidAlice {
		t.Fatalf("lookup received %v, want the device-less %v", seen, lidAlice)
	}
}

func TestDMAltPicksTheAddressOfTheConversationPartner(t *testing.T) {
	sender, recipient := pnAlice, pnBob
	incoming := types.MessageSource{SenderAlt: sender, RecipientAlt: recipient}
	outgoing := types.MessageSource{IsFromMe: true, SenderAlt: sender, RecipientAlt: recipient}

	if got := dmAlt(incoming); got != sender {
		t.Errorf("incoming: dmAlt = %v, want the sender alt %v", got, sender)
	}
	if got := dmAlt(outgoing); got != recipient {
		t.Errorf("outgoing: dmAlt = %v, want the recipient alt %v", got, recipient)
	}
}

func TestIsStatusBroadcast(t *testing.T) {
	tests := []struct {
		name string
		jid  types.JID
		want bool
	}{
		{"status broadcast", types.StatusBroadcastJID, true},
		{"status broadcast with device", withDevice(types.StatusBroadcastJID, 2), true},
		{"other broadcast list", types.NewJID("12345", types.BroadcastServer), false},
		{"user named status", types.NewJID("status", types.DefaultUserServer), false},
		{"group", group, false},
		{"empty", types.EmptyJID, false},
	}
	for _, tt := range tests {
		if got := isStatusBroadcast(tt.jid); got != tt.want {
			t.Errorf("%s: isStatusBroadcast(%v) = %v, want %v", tt.name, tt.jid, got, tt.want)
		}
	}
}

func TestChatFor(t *testing.T) {
	tests := []struct {
		name string
		jid  types.JID
		chat string
		want wa.Chat
	}{
		{"group", group, "Family", wa.Chat{JID: "120363000000000001@g.us", Name: "Family", IsGroup: true}},
		{"direct chat", pnAlice, "Alice", wa.Chat{JID: "33611111111@s.whatsapp.net", Name: "Alice"}},
		{"direct chat without name", pnAlice, "", wa.Chat{JID: "33611111111@s.whatsapp.net"}},
		{"newsletter is not a group", types.NewJID("1203", types.NewsletterServer), "News", wa.Chat{JID: "1203@newsletter", Name: "News"}},
		{"broadcast list is not a group", types.NewJID("99", types.BroadcastServer), "", wa.Chat{JID: "99@broadcast"}},
		{"lid chat", lidAlice, "Hidden", wa.Chat{JID: "98765432101234@lid", Name: "Hidden"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chatFor(tt.jid, tt.chat); got != tt.want {
				t.Fatalf("chatFor = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestContactFrom(t *testing.T) {
	tests := []struct {
		name   string
		jid    types.JID
		info   types.ContactInfo
		want   wa.Contact
		wantOK bool
	}{
		{"full name preferred", pnAlice, types.ContactInfo{FullName: "Alice Smith", FirstName: "Alice", PushName: "Ali"},
			wa.Contact{JID: "33611111111@s.whatsapp.net", Name: "Alice Smith", FirstName: "Alice", PushName: "Ali"}, true},
		{"first name used when no full name", pnBob, types.ContactInfo{FirstName: "Bob", PushName: "Bobby"},
			wa.Contact{JID: "33622222222@s.whatsapp.net", Name: "Bob", FirstName: "Bob", PushName: "Bobby"}, true},
		{"push name only keeps an empty saved name", pnAlice, types.ContactInfo{PushName: "Ali"},
			wa.Contact{JID: "33611111111@s.whatsapp.net", PushName: "Ali"}, true},
		{"saved name without push name", pnAlice, types.ContactInfo{FullName: "Alice"},
			wa.Contact{JID: "33611111111@s.whatsapp.net", Name: "Alice"}, true},
		{"business name alone is kept apart from the saved name", pnAlice, types.ContactInfo{BusinessName: "ACME"},
			wa.Contact{JID: "33611111111@s.whatsapp.net", BusinessName: "ACME"}, true},
		{"first name is reported on its own too", pnAlice, types.ContactInfo{FullName: "Alice Smith", FirstName: "Alice"},
			wa.Contact{JID: "33611111111@s.whatsapp.net", Name: "Alice Smith", FirstName: "Alice"}, true},
		{"no names at all", pnAlice, types.ContactInfo{Found: true}, wa.Contact{}, false},
		{"empty info", pnAlice, types.ContactInfo{}, wa.Contact{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := contactFrom(tt.jid, tt.info)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("contactFrom = (%+v, %v), want (%+v, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
