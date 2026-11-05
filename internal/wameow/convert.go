package wameow

import (
	"cmp"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
	"slices"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

const maxUnwrapDepth = 4

// uploaded mirrors the fields of whatsmeow.UploadResponse that end up in an
// outgoing media message.
type uploaded struct {
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileEncSHA256 []byte
	FileSHA256    []byte
	FileLength    uint64
}

func whatsmeowMediaType(kind wa.MediaType) whatsmeow.MediaType {
	switch kind {
	case wa.MediaImage, wa.MediaSticker:
		return whatsmeow.MediaImage
	case wa.MediaVideo:
		return whatsmeow.MediaVideo
	case wa.MediaAudio:
		return whatsmeow.MediaAudio
	default:
		return whatsmeow.MediaDocument
	}
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return proto.String(s)
}

// buildOutgoingMedia assembles the message carrying an uploaded attachment.
// WhatsApp has no caption field for audio, so the caption is dropped there.
func buildOutgoingMedia(kind wa.MediaType, up uploaded, f wa.OutgoingFile) *waE2E.Message {
	mimeType := f.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	switch kind {
	case wa.MediaImage:
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String(mimeType),
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(up.FileLength),
			Caption:       optString(f.Caption),
		}}
	case wa.MediaVideo:
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String(mimeType),
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(up.FileLength),
			Caption:       optString(f.Caption),
		}}
	case wa.MediaAudio:
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String(mimeType),
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(up.FileLength),
		}}
	default:
		return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			URL:           proto.String(up.URL),
			DirectPath:    proto.String(up.DirectPath),
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String(mimeType),
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    proto.Uint64(up.FileLength),
			FileName:      optString(f.FileName),
			Title:         optString(f.FileName),
			Caption:       optString(f.Caption),
		}}
	}
}

// unwrapMessage peels the wrappers that only add delivery metadata. View-once
// wrappers are deliberately left in place so their content is never read.
func unwrapMessage(msg *waE2E.Message) *waE2E.Message {
	for range maxUnwrapDepth {
		var inner *waE2E.Message
		switch {
		case msg.GetDeviceSentMessage().GetMessage() != nil:
			inner = msg.GetDeviceSentMessage().GetMessage()
		case msg.GetEphemeralMessage().GetMessage() != nil:
			inner = msg.GetEphemeralMessage().GetMessage()
		case msg.GetDocumentWithCaptionMessage().GetMessage() != nil:
			inner = msg.GetDocumentWithCaptionMessage().GetMessage()
		}
		if inner == nil {
			return msg
		}
		msg = inner
	}
	return msg
}

// extractContent returns the text (or caption) and the media metadata carried
// by msg. Both are empty when the message holds neither, e.g. reactions.
func extractContent(msg *waE2E.Message) (string, *wa.Media) {
	msg = unwrapMessage(msg)
	switch {
	case msg.GetImageMessage() != nil:
		m := msg.GetImageMessage()
		return m.GetCaption(), &wa.Media{
			Type:          wa.MediaImage,
			MimeType:      m.GetMimetype(),
			DirectPath:    m.GetDirectPath(),
			URL:           m.GetURL(),
			MediaKey:      m.GetMediaKey(),
			FileSHA256:    m.GetFileSHA256(),
			FileEncSHA256: m.GetFileEncSHA256(),
			FileLength:    m.GetFileLength(),
		}
	case msg.GetVideoMessage() != nil:
		m := msg.GetVideoMessage()
		return m.GetCaption(), &wa.Media{
			Type:          wa.MediaVideo,
			MimeType:      m.GetMimetype(),
			DirectPath:    m.GetDirectPath(),
			URL:           m.GetURL(),
			MediaKey:      m.GetMediaKey(),
			FileSHA256:    m.GetFileSHA256(),
			FileEncSHA256: m.GetFileEncSHA256(),
			FileLength:    m.GetFileLength(),
		}
	case msg.GetAudioMessage() != nil:
		m := msg.GetAudioMessage()
		return "", &wa.Media{
			Type:          wa.MediaAudio,
			MimeType:      m.GetMimetype(),
			DirectPath:    m.GetDirectPath(),
			URL:           m.GetURL(),
			MediaKey:      m.GetMediaKey(),
			FileSHA256:    m.GetFileSHA256(),
			FileEncSHA256: m.GetFileEncSHA256(),
			FileLength:    m.GetFileLength(),
		}
	case msg.GetDocumentMessage() != nil:
		m := msg.GetDocumentMessage()
		name := m.GetFileName()
		if name == "" {
			name = m.GetTitle()
		}
		return m.GetCaption(), &wa.Media{
			Type:          wa.MediaDocument,
			MimeType:      m.GetMimetype(),
			FileName:      name,
			DirectPath:    m.GetDirectPath(),
			URL:           m.GetURL(),
			MediaKey:      m.GetMediaKey(),
			FileSHA256:    m.GetFileSHA256(),
			FileEncSHA256: m.GetFileEncSHA256(),
			FileLength:    m.GetFileLength(),
		}
	case msg.GetStickerMessage() != nil:
		m := msg.GetStickerMessage()
		return "", &wa.Media{
			Type:          wa.MediaSticker,
			MimeType:      m.GetMimetype(),
			DirectPath:    m.GetDirectPath(),
			URL:           m.GetURL(),
			MediaKey:      m.GetMediaKey(),
			FileSHA256:    m.GetFileSHA256(),
			FileEncSHA256: m.GetFileEncSHA256(),
			FileLength:    m.GetFileLength(),
		}
	case msg.GetExtendedTextMessage() != nil:
		return msg.GetExtendedTextMessage().GetText(), nil
	default:
		return msg.GetConversation(), nil
	}
}

// editOf recognises an edit of an earlier message: a MESSAGE_EDIT protocol
// message, possibly arriving inside an edit wrapper (isWrapped, as reported by
// whatsmeow). It returns the id of the edited message and the message holding
// the new content. isEdit is true even when no usable target is present, so
// that such events are never stored as new messages.
func editOf(info types.MessageInfo, msg *waE2E.Message, isWrapped bool) (targetID string, content *waE2E.Message, isEdit bool) {
	pm := unwrapMessage(msg).GetProtocolMessage()
	if pm.GetType() == waE2E.ProtocolMessage_MESSAGE_EDIT {
		return pm.GetKey().GetID(), pm.GetEditedMessage(), true
	}
	if isWrapped {
		return info.MsgBotInfo.EditTargetID, msg, true
	}
	return "", nil, false
}

// convertEdit builds the edit event for an incoming message. ok is false when
// the message is not an edit or carries nothing to apply; isEdit tells the
// caller to drop the message rather than store it.
func convertEdit(info types.MessageInfo, msg *waE2E.Message, isWrapped bool, chat, sender types.JID) (ev wa.MessageEditEvent, isEdit, ok bool) {
	targetID, content, isEdit := editOf(info, msg, isWrapped)
	if !isEdit {
		return wa.MessageEditEvent{}, false, false
	}
	text, _ := extractContent(content)
	if targetID == "" || text == "" {
		return wa.MessageEditEvent{}, true, false
	}
	return wa.MessageEditEvent{
		ChatJID:   chat.String(),
		ID:        targetID,
		SenderJID: sender.String(),
		Text:      text,
	}, true, true
}

// convertMessage builds a wa.Message from whatsmeow message metadata. chat and
// sender are the already-resolved JIDs. ok is false when the message carries
// no text and no media.
func convertMessage(info types.MessageInfo, msg *waE2E.Message, chat, sender types.JID) (wa.Message, bool) {
	text, media := extractContent(msg)
	if text == "" && media == nil {
		return wa.Message{}, false
	}
	out := wa.Message{
		ID:        info.ID,
		ChatJID:   chat.String(),
		SenderJID: sender.String(),
		Timestamp: info.Timestamp.UTC(),
		FromMe:    info.IsFromMe,
		Text:      text,
		Media:     media,
	}
	if !info.IsFromMe {
		out.SenderName = info.PushName
	}
	return out, true
}

// preferPN maps a LID jid to its phone-number form using alt when it is a
// phone-number JID, otherwise lookup. The input is returned (without device
// suffix) when no phone number is known.
func preferPN(jid, alt types.JID, lookup func(types.JID) (types.JID, bool)) types.JID {
	jid = jid.ToNonAD()
	if jid.Server != types.HiddenUserServer {
		return jid
	}
	if alt.Server == types.DefaultUserServer && alt.User != "" {
		return alt.ToNonAD()
	}
	if lookup != nil {
		if pn, ok := lookup(jid); ok && pn.Server == types.DefaultUserServer && pn.User != "" {
			return pn.ToNonAD()
		}
	}
	return jid
}

// dmAlt selects the alternative address that belongs to the chat of a
// one-to-one message.
func dmAlt(src types.MessageSource) types.JID {
	if src.IsFromMe {
		return src.RecipientAlt
	}
	return src.SenderAlt
}

func isStatusBroadcast(jid types.JID) bool {
	return jid.User == types.StatusBroadcastJID.User && jid.Server == types.BroadcastServer
}

func chatFor(jid types.JID, name string) wa.Chat {
	return wa.Chat{JID: jid.String(), Name: name, IsGroup: jid.Server == types.GroupServer}
}

// dmChatNames derives names for one-to-one chats from the merged contacts.
// A chat may be stored under the phone-number or the LID JID, so a name is
// emitted for both; the store only fills chats that exist and have no name.
func dmChatNames(contacts []wa.Contact) []wa.Chat {
	var out []wa.Chat
	for _, c := range contacts {
		name := cmp.Or(c.Name, c.BusinessName, c.PushName)
		if name == "" {
			continue
		}
		for _, jid := range uniqueNonEmpty(c.PN, c.LID, c.JID) {
			out = append(out, wa.Chat{JID: jid, Name: name})
		}
	}
	slices.SortFunc(out, func(a, b wa.Chat) int { return cmp.Compare(a.JID, b.JID) })
	return slices.CompactFunc(out, func(a, b wa.Chat) bool { return a.JID == b.JID })
}

func uniqueNonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// contactFrom builds a contact from one store row; ok is false when the row
// carries no name at all.
func contactFrom(jid types.JID, info types.ContactInfo) (wa.Contact, bool) {
	name := cmp.Or(info.FullName, info.FirstName)
	if name == "" && info.BusinessName == "" && info.PushName == "" {
		return wa.Contact{}, false
	}
	return wa.Contact{
		JID:          jid.String(),
		Name:         name,
		FirstName:    info.FirstName,
		BusinessName: info.BusinessName,
		PushName:     info.PushName,
	}, true
}
