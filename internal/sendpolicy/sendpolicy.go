// Package sendpolicy decides whether an outgoing message may be sent.
package sendpolicy

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/ratelimit"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

// Errors returned by Policy.Check.
var (
	ErrSendDisabled   = errors.New("sending is disabled on this server")
	ErrNotAllowlisted = errors.New("recipient is not in the send allowlist")
	ErrRateLimited    = errors.New("send rate limit reached")
)

// Allowlist is the set of recipients a message may be sent to. The zero value
// allows everyone.
type Allowlist struct {
	entries map[string]struct{}
}

// ParseAllowlist builds an Allowlist from JIDs and phone numbers. Blank
// entries are skipped. Phone numbers (international format, "+" or "00"
// optional) match the user JID on s.whatsapp.net only; JID entries match their
// own server only.
func ParseAllowlist(entries []string) (Allowlist, error) {
	a := Allowlist{entries: map[string]struct{}{}}
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "@") {
			if !wa.ValidJID(entry) {
				return Allowlist{}, fmt.Errorf("invalid allowlist JID %q", entry)
			}
			a.entries[canonicalJID(entry)] = struct{}{}
			continue
		}
		digits, ok := wa.NormalizePhone(entry)
		if !ok {
			return Allowlist{}, fmt.Errorf("invalid allowlist entry %q: not a JID or phone number", entry)
		}
		a.entries[wa.UserJID(digits)] = struct{}{}
	}
	return a, nil
}

// canonicalJID strips the ":device" and ".agent" suffixes from the user part of
// non-group JIDs. Group JIDs are compared exactly.
func canonicalJID(jid string) string {
	if wa.IsGroup(jid) {
		return jid
	}
	return wa.User(jid) + "@" + wa.Server(jid)
}

// Empty reports whether the allowlist is unrestricted.
func (a Allowlist) Empty() bool { return len(a.entries) == 0 }

// Allows reports whether jid may receive messages. The user and server parts
// must both match; device and agent suffixes are ignored except on groups.
func (a Allowlist) Allows(jid string) bool {
	if a.Empty() {
		return true
	}
	_, ok := a.entries[canonicalJID(jid)]
	return ok
}

// Policy gates every send.
type Policy struct {
	Enabled   bool
	Allowlist Allowlist
	Limiter   *ratelimit.Limiter
}

// Check is CheckSlot without the slot, for callers that never refund.
func (p *Policy) Check(jid string) error {
	_, err := p.CheckSlot(jid)
	return err
}

// CheckSlot returns nil when a message to jid may be sent now, consuming a rate
// slot that Refund can give back. The allowlist is evaluated before the limiter
// so rejected recipients never use up a slot. The returned slot is the zero
// value when no limiter is configured.
func (p *Policy) CheckSlot(jid string) (ratelimit.Slot, error) {
	return p.checkSlot(jid, false)
}

// CheckStatusSlot applies send enablement and rate limits without a chat allowlist.
// WhatsApp status privacy determines the story audience.
func (p *Policy) CheckStatusSlot() (ratelimit.Slot, error) {
	return p.checkSlot("status@broadcast", true)
}

func (p *Policy) checkSlot(jid string, status bool) (ratelimit.Slot, error) {
	if !p.Enabled {
		return ratelimit.Slot{}, ErrSendDisabled
	}
	if !status && !p.Allowlist.Allows(jid) {
		return ratelimit.Slot{}, ErrNotAllowlisted
	}
	if p.Limiter == nil {
		return ratelimit.Slot{}, nil
	}
	ok, retry, slot := p.Limiter.Reserve()
	if !ok {
		secs := int((retry + time.Second - 1) / time.Second)
		return ratelimit.Slot{}, fmt.Errorf("%w: retry after %ds", ErrRateLimited, secs)
	}
	return slot, nil
}

// Refund gives back the rate slot consumed by a successful CheckSlot when the
// send itself then failed before reaching the network. It is safe on a nil
// Policy or a nil Limiter.
func (p *Policy) Refund(slot ratelimit.Slot) {
	if p == nil || p.Limiter == nil {
		return
	}
	p.Limiter.Refund(slot)
}
