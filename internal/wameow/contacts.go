package wameow

import (
	"cmp"
	"context"
	"maps"
	"slices"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/fmflurry/whatsapp-mcp/internal/redact"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

// whatsmeowAppState drives app state fetches through the whatsmeow client.
type whatsmeowAppState struct{ c *Client }

func (b whatsmeowAppState) LoggedIn() bool {
	return b.c.cli.Store.ID != nil && b.c.cli.IsConnected() && b.c.cli.IsLoggedIn()
}

func (b whatsmeowAppState) PatchVersion(ctx context.Context, name appstate.WAPatchName) (uint64, error) {
	version, _, err := b.c.cli.Store.AppState.GetAppStateVersion(ctx, string(name))
	return version, err
}

// FetchPatch counts the mutations whatsmeow dispatches while fetching. The
// count also includes mutations of a fetch whatsmeow runs concurrently on its
// own, so it is approximate.
func (b whatsmeowAppState) FetchPatch(ctx context.Context, name appstate.WAPatchName, fullSync bool) (int, error) {
	before := b.c.appStateMutations.Load()
	err := b.c.cli.FetchAppState(ctx, name, fullSync, false)
	return int(b.c.appStateMutations.Load() - before), err
}

func (b whatsmeowAppState) ContactCounts(ctx context.Context) (named, total int, err error) {
	all, err := b.c.cli.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, info := range all {
		if info.FullName != "" || info.FirstName != "" {
			named++
		}
	}
	return named, len(all), nil
}

// ResyncContacts implements wa.Client.
func (c *Client) ResyncContacts(context.Context) (wa.ResyncOutcome, error) {
	if c.cli.Store.ID == nil {
		return "", wa.ErrNotPaired
	}
	if !c.cli.IsConnected() || !c.cli.IsLoggedIn() {
		return "", wa.ErrNotConnected
	}
	return c.sync.startManual(c.lifeCtx), nil
}

// ContactSync implements wa.Client.
func (c *Client) ContactSync(ctx context.Context) (wa.ContactSyncStatus, error) {
	if c.cli.Store.ID == nil {
		return wa.ContactSyncStatus{}, wa.ErrNotPaired
	}
	return c.sync.status(ctx), nil
}

// Contacts implements wa.Client.
func (c *Client) Contacts(ctx context.Context) ([]wa.Contact, error) {
	if c.cli.Store.ID == nil {
		return nil, wa.ErrNotPaired
	}
	all, err := c.cli.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return nil, scrubbed("list contacts", err)
	}
	r := c.newResolver()
	return mergeContacts(all, r.lookup, r.lidFor), nil
}

// onContactAction records the LID/phone-number pair an address-book entry
// carries; the entry's name itself is stored by whatsmeow.
func (c *Client) onContactAction(evt *events.Contact) {
	jid := evt.JID.ToNonAD()
	for _, alt := range []string{evt.Action.GetLidJID(), evt.Action.GetPnJID()} {
		if alt == "" {
			continue
		}
		other, err := types.ParseJID(alt)
		if err != nil || other.User == "" {
			continue
		}
		c.cli.StoreLIDPNMapping(c.lifeCtx, jid, other.ToNonAD())
	}
}

func (c *Client) onAppStateSyncComplete(evt *events.AppStateSyncComplete) {
	c.log.Info("app state full sync complete", "patch", string(evt.Name), "version", evt.Version, "recovery", evt.Recovery)
	if evt.Name == appstate.WAPatchCriticalUnblockLow {
		go c.publishContactNames()
	}
}

func (c *Client) onAppStateSyncError(evt *events.AppStateSyncError) {
	c.log.Warn("app state sync error", "patch", string(evt.Name), "full_sync", evt.FullSync,
		"error", redact.Scrub(fmtErr(evt.Error)))
}

func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type contactAcc struct {
	pn, lid types.JID
	info    types.ContactInfo
}

// mergeContacts folds the store's contact rows into one contact per person:
// a LID row and a phone-number row of the same person are merged, the
// phone-number row's values winning. pnForLID and lidForPN consult the LID map.
func mergeContacts(all map[types.JID]types.ContactInfo,
	pnForLID, lidForPN func(types.JID) (types.JID, bool)) []wa.Contact {
	jids := slices.SortedFunc(maps.Keys(all), func(a, b types.JID) int {
		if a.Server != b.Server {
			return cmp.Compare(serverRank(a.Server), serverRank(b.Server))
		}
		return cmp.Compare(a.String(), b.String())
	})
	byKey := make(map[types.JID]*contactAcc, len(jids))
	order := make([]types.JID, 0, len(jids))
	for _, raw := range jids {
		jid := raw.ToNonAD()
		var pn, lid types.JID
		switch jid.Server {
		case types.DefaultUserServer:
			pn = jid
			if l, ok := lidForPN(jid); ok && l.Server == types.HiddenUserServer {
				lid = l.ToNonAD()
			}
		case types.HiddenUserServer:
			lid = jid
			if p, ok := pnForLID(jid); ok && p.Server == types.DefaultUserServer {
				pn = p.ToNonAD()
			}
		default:
			continue
		}
		key := cmp.Or(pn, lid)
		acc, ok := byKey[key]
		if !ok {
			acc = &contactAcc{}
			byKey[key] = acc
			order = append(order, key)
		}
		acc.pn = cmp.Or(acc.pn, pn)
		acc.lid = cmp.Or(acc.lid, lid)
		info := all[raw]
		acc.info.FullName = cmp.Or(acc.info.FullName, info.FullName)
		acc.info.FirstName = cmp.Or(acc.info.FirstName, info.FirstName)
		acc.info.BusinessName = cmp.Or(acc.info.BusinessName, info.BusinessName)
		acc.info.PushName = cmp.Or(acc.info.PushName, info.PushName)
	}
	out := make([]wa.Contact, 0, len(order))
	for _, key := range order {
		acc := byKey[key]
		if contact, ok := contactFrom(key, acc.info); ok {
			contact.PN = jidString(acc.pn)
			contact.LID = jidString(acc.lid)
			out = append(out, contact)
		}
	}
	slices.SortFunc(out, func(a, b wa.Contact) int { return cmp.Compare(a.JID, b.JID) })
	return out
}

func serverRank(server string) int {
	if server == types.DefaultUserServer {
		return 0
	}
	return 1
}

func jidString(j types.JID) string {
	if j.IsEmpty() {
		return ""
	}
	return j.String()
}
