package wa

// MessageEditEvent carries the new text of an already delivered message. It
// only applies to a stored message of the same chat sent by SenderJID.
type MessageEditEvent struct {
	ChatJID   string
	ID        string
	SenderJID string
	Text      string
}

// ContactNamesEvent carries names for one-to-one chats derived from the
// address book. Consumers only fill in names of chats they already know.
type ContactNamesEvent struct{ Chats []Chat }

func (MessageEditEvent) isEvent()  {}
func (ContactNamesEvent) isEvent() {}
