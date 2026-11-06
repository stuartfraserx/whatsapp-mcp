package wameow

import (
	"context"
	"fmt"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/fmflurry/whatsapp-mcp/internal/redact"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

const groupListTimeout = 30 * time.Second

// pnResolver maps between LID and phone-number JIDs, caching store lookups
// for the duration of one event.
type pnResolver struct {
	c        *Client
	cache    map[types.JID]types.JID
	lidCache map[types.JID]types.JID
}

func (c *Client) newResolver() *pnResolver {
	return &pnResolver{c: c, cache: make(map[types.JID]types.JID), lidCache: make(map[types.JID]types.JID)}
}

func (r *pnResolver) pn(jid, alt types.JID) types.JID {
	return preferPN(jid, alt, r.lookup)
}

func (r *pnResolver) lookup(lid types.JID) (types.JID, bool) {
	if pn, ok := r.cache[lid]; ok {
		return pn, !pn.IsEmpty()
	}
	lids := r.c.cli.Store.LIDs
	if lids == nil {
		return types.EmptyJID, false
	}
	pn, err := lids.GetPNForLID(r.c.lifeCtx, lid)
	if err != nil || pn.IsEmpty() {
		pn = types.EmptyJID
	}
	r.cache[lid] = pn
	return pn, !pn.IsEmpty()
}

func (r *pnResolver) lidFor(pn types.JID) (types.JID, bool) {
	if lid, ok := r.lidCache[pn]; ok {
		return lid, !lid.IsEmpty()
	}
	lids := r.c.cli.Store.LIDs
	if lids == nil {
		return types.EmptyJID, false
	}
	lid, err := lids.GetLIDForPN(r.c.lifeCtx, pn)
	if err != nil || lid.IsEmpty() {
		lid = types.EmptyJID
	}
	r.lidCache[pn] = lid
	return lid, !lid.IsEmpty()
}

func (c *Client) ownJID() types.JID {
	return c.cli.Store.GetJID().ToNonAD()
}

func (c *Client) handleEvent(raw any) {
	switch evt := raw.(type) {
	case *events.Message:
		c.onMessage(evt)
	case *events.HistorySync:
		c.onHistorySync(evt)
	case *events.Connected:
		c.onConnected()
	case *events.StreamReplaced:
		c.log.Warn("whatsapp stream replaced by another client using the same session; reconnecting with backoff")
		c.reconnect.replaced(c.lifeCtx)
		c.emit(wa.StreamReplacedEvent{})
	case *events.KeepAliveTimeout:
		c.log.Warn("whatsapp keepalive timeout")
	case *events.ConnectFailure:
		c.log.Warn("whatsapp connection failure", "reason", redact.Scrub(evt.Reason.String()))
		c.reconnect.disconnected("connect_failure")
	case *events.Disconnected:
		if c.reconnect != nil {
			c.reconnect.disconnected("disconnected")
		}
		c.setConnecting(false)
		c.emit(wa.DisconnectedEvent{})
	case *events.LoggedOut:
		c.onLoggedOut(evt)
	case *events.PairSuccess:
		c.onPaired(evt.ID.ToNonAD().String())
	case *events.GroupInfo:
		if evt.Name != nil && evt.Name.Name != "" {
			c.emit(wa.ChatInfoEvent{Chats: []wa.Chat{chatFor(evt.JID, evt.Name.Name)}})
		}
	case *events.JoinedGroup:
		if evt.Name != "" {
			c.emit(wa.ChatInfoEvent{Chats: []wa.Chat{chatFor(evt.JID, evt.Name)}})
		}
	case *events.AppState:
		c.appStateMutations.Add(1)
	case *events.Contact:
		c.onContactAction(evt)
	case *events.AppStateSyncComplete:
		c.onAppStateSyncComplete(evt)
	case *events.AppStateSyncError:
		c.onAppStateSyncError(evt)
	}
}

func (c *Client) onMessage(evt *events.Message) {
	if evt.IsViewOnce || isStatusBroadcast(evt.Info.Chat) {
		return
	}
	info := evt.Info
	r := c.newResolver()
	chat := r.pn(info.Chat, dmAlt(info.MessageSource))
	sender := c.senderJID(r, info)
	if edit, isEdit, ok := convertEdit(info, evt.Message, evt.IsEdit, chat, sender); isEdit {
		if ok {
			c.emit(edit)
		}
		return
	}
	m, ok := convertMessage(info, evt.Message, chat, sender)
	if !ok {
		return
	}
	c.emit(wa.MessageEvent{Message: m})
}

func (c *Client) senderJID(r *pnResolver, info types.MessageInfo) types.JID {
	if info.IsFromMe {
		if own := c.ownJID(); !own.IsEmpty() {
			return own
		}
	}
	return r.pn(info.Sender, info.SenderAlt)
}

func (c *Client) onHistorySync(evt *events.HistorySync) {
	r := c.newResolver()
	var out wa.HistoryEvent
	skipped := 0
	for _, conv := range evt.Data.GetConversations() {
		rawChat, err := types.ParseJID(conv.GetID())
		if err != nil || rawChat.User == "" || isStatusBroadcast(rawChat) {
			continue
		}
		chat := r.pn(rawChat, historyAlt(conv.GetPnJID()))
		name := conv.GetName()
		if name == "" {
			name = conv.GetDisplayName()
		}
		out.Chats = append(out.Chats, chatFor(chat, name))
		for _, hm := range conv.GetMessages() {
			web := hm.GetMessage()
			if web == nil {
				continue
			}
			parsed, err := c.cli.ParseWebMessage(rawChat, web)
			if err != nil {
				skipped++
				continue
			}
			if parsed.IsViewOnce {
				continue
			}
			if _, _, isEdit := editOf(parsed.Info, parsed.Message, parsed.IsEdit); isEdit {
				continue
			}
			if m, ok := convertMessage(parsed.Info, parsed.Message, chat, c.senderJID(r, parsed.Info)); ok {
				out.Messages = append(out.Messages, m)
			}
		}
	}
	if skipped > 0 {
		c.log.Debug("history sync: skipped unparseable messages", "count", skipped)
	}
	if len(out.Chats) == 0 && len(out.Messages) == 0 {
		return
	}
	c.emit(out)
}

func historyAlt(pn string) types.JID {
	if pn == "" {
		return types.EmptyJID
	}
	jid, err := types.ParseJID(pn)
	if err != nil {
		return types.EmptyJID
	}
	return jid
}

func (c *Client) onConnected() {
	c.setConnected()
	if c.reconnect != nil {
		c.reconnect.connected()
	}
	c.emit(wa.ConnectedEvent{})
	go c.publishGroupNames()
	go c.publishContactNames()
	c.sync.startAuto(c.lifeCtx)
}

func (c *Client) publishContactNames() {
	ctx, cancel := context.WithTimeout(c.lifeCtx, groupListTimeout)
	defer cancel()
	contacts, err := c.Contacts(ctx)
	if err != nil {
		c.log.Warn("list contacts failed", "error", redact.Scrub(err.Error()))
		return
	}
	if chats := dmChatNames(contacts); len(chats) > 0 {
		c.emit(wa.ContactNamesEvent{Chats: chats})
	}
}

func (c *Client) publishGroupNames() {
	ctx, cancel := context.WithTimeout(c.lifeCtx, groupListTimeout)
	defer cancel()
	groups, err := c.cli.GetJoinedGroups(ctx)
	if err != nil {
		c.log.Warn("list joined groups failed", "error", redact.Scrub(err.Error()))
		return
	}
	chats := make([]wa.Chat, 0, len(groups))
	for _, g := range groups {
		if g == nil || g.Name == "" {
			continue
		}
		chats = append(chats, chatFor(g.JID, g.Name))
	}
	if len(chats) > 0 {
		c.emit(wa.ChatInfoEvent{Chats: chats})
	}
}

func (c *Client) onLoggedOut(evt *events.LoggedOut) {
	c.setLoggedOut()
	if c.reconnect != nil {
		c.reconnect.mu.Lock()
		c.reconnect.loggedOut = true
		c.reconnect.reason = "logged_out"
		c.reconnect.connectedAt = time.Time{}
		c.reconnect.mu.Unlock()
	}
	c.emit(wa.LoggedOutEvent{Reason: fmt.Sprint(evt.Reason)})
}
