package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/fmflurry/whatsapp-mcp/internal/redact"
	"github.com/fmflurry/whatsapp-mcp/internal/store"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

const (
	maxCandidates = 10
	minPhoneQuery = 3
	chatScanPages = 10
	// listChatsScanPages bounds the in-memory query of list_chats to 10,000
	// chats.
	listChatsScanPages = 50
	chatScanPerPage    = maxLimit
)

// Sources of a name match, in order of trust.
const (
	sourceContact  = "contact"
	sourcePushName = "push_name"
	sourceChat     = "chat"
)

type candidate struct {
	JID    string `json:"jid"`
	Name   string `json:"name"`
	Source string `json:"source"`
}

// scanChats pages through every stored chat, up to the scan cap, without a
// store-side filter: the store's LIKE folds only ASCII case, so matching is done
// by the caller with the same Unicode folding it applies to contacts. truncated
// reports that the cap was reached before the last page.
func (h *handlers) scanChats(ctx context.Context, sortBy string, maxPages int) (chats []store.ChatSummary, truncated bool, err error) {
	for page := 0; page < maxPages; page++ {
		batch, more, err := h.Store.ListChats(ctx, store.ChatQuery{Limit: chatScanPerPage, Offset: page * chatScanPerPage, SortBy: sortBy})
		if err != nil {
			return nil, false, fmt.Errorf("list chats: %w", err)
		}
		chats = append(chats, batch...)
		if !more {
			return chats, false, nil
		}
	}
	return chats, true, nil
}

// resolution is the outcome of resolving a send recipient: either a JID or a
// set of candidates the caller must disambiguate.
type resolution struct {
	JID        string
	Candidates []candidate
}

// resolveRecipient maps a JID, an international phone number or a saved
// contact name to a JID. Names that are not an unambiguous saved-contact match
// return candidates and no JID; unknown names are errors.
func (h *handlers) resolveRecipient(ctx context.Context, recipient string) (resolution, error) {
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		return resolution{}, errors.New("recipient is required")
	}

	if strings.Contains(recipient, "@") {
		return resolveJID(recipient)
	}
	if digits, ok := wa.NormalizePhone(recipient); ok {
		if !strings.HasPrefix(recipient, "+") && !strings.HasPrefix(recipient, "00") {
			return resolution{}, errors.New("phone number must include the country code, e.g. +33612345678")
		}
		return resolution{JID: wa.UserJID(digits)}, nil
	}

	return h.resolveName(ctx, recipient)
}

func resolveJID(recipient string) (resolution, error) {
	if !wa.ValidJID(recipient) {
		return resolution{}, errors.New("recipient is not a valid WhatsApp JID")
	}
	user, _, _ := strings.Cut(recipient, "@")
	if user != wa.User(recipient) {
		return resolution{}, errors.New("recipient JID must not carry a device or agent suffix; use the plain JID")
	}
	switch wa.Server(recipient) {
	case wa.ServerUser, wa.ServerGroup, wa.ServerLID:
		return resolution{JID: recipient}, nil
	default:
		return resolution{}, errors.New("recipient must be a user or group JID")
	}
}

type nameMatch struct {
	candidate
	exact bool
}

// resolveName auto-resolves only a case-insensitive exact match of one saved
// contact name when nothing else matches, even partially. Push names and chat
// or group names are attacker-influenced, so they are only ever offered as
// candidates.
func (h *handlers) resolveName(ctx context.Context, name string) (resolution, error) {
	contacts, err := h.Client.Contacts(ctx)
	if err != nil {
		return resolution{}, fmt.Errorf("load contacts: %w", err)
	}
	chats, truncated, err := h.scanChats(ctx, "", chatScanPages)
	if err != nil {
		return resolution{}, err
	}
	if truncated {
		return resolution{}, errors.New("too many chats to check this name safely; use search_contacts or pass a JID or phone number")
	}

	lower := strings.ToLower(name)
	matches := map[string]nameMatch{}
	savedExact := map[string]struct{}{}
	people := newDirectory(contacts)
	add := func(jid, label, source string) {
		l := strings.ToLower(strings.TrimSpace(label))
		if l == "" || !strings.Contains(l, lower) {
			return
		}
		// A person's LID chat and phone-number contact are one recipient.
		if p := people.lookup(jid); p != nil {
			jid = p.jid
		}
		m := nameMatch{candidate: candidate{JID: jid, Name: label, Source: source}, exact: l == lower}
		if source == sourceContact && m.exact {
			savedExact[jid] = struct{}{}
		}
		if prev, ok := matches[jid]; !ok || (m.exact && !prev.exact) {
			matches[jid] = m
		}
	}
	for _, c := range contacts {
		if c.JID == "" || wa.IsGroup(c.JID) {
			continue
		}
		add(c.JID, c.Name, sourceContact)
		add(c.JID, c.PushName, sourcePushName)
	}
	for _, c := range chats {
		add(c.JID, c.Name, sourceChat)
	}

	if len(matches) == 0 {
		return resolution{}, errors.New("no contact or chat matches this recipient; use search_contacts or pass a JID or phone number")
	}
	if len(matches) == 1 && len(savedExact) == 1 {
		for jid := range savedExact {
			h.Log.Debug("recipient resolved by saved contact name", "jid", redact.JID(jid))
			return resolution{JID: jid}, nil
		}
	}
	return resolution{Candidates: sortedCandidates(matches)}, nil
}

func sortedCandidates(m map[string]nameMatch) []candidate {
	out := make([]candidate, 0, len(m))
	for _, c := range m {
		out = append(out, c.candidate)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].JID < out[j].JID
	})
	if len(out) > maxCandidates {
		out = out[:maxCandidates]
	}
	return out
}
