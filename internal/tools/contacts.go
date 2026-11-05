package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/text/unicode/norm"

	"github.com/fmflurry/whatsapp-mcp/internal/redact"
	"github.com/fmflurry/whatsapp-mcp/internal/store"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

type searchContactsInput struct {
	Query string `json:"query" jsonschema:"Name, profile name or phone number to search for. Accents and case are ignored and every word must match, in any order"`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum contacts to return, 1-200 (default 20)"`
}

type contactOut struct {
	JID      string `json:"jid"`
	Name     string `json:"name"`
	PushName string `json:"push_name,omitempty"`
	Phone    string `json:"phone,omitempty"`
	PN       string `json:"pn,omitempty"`
	LID      string `json:"lid,omitempty"`
}

type searchContactsOutput struct {
	Contacts []contactOut `json:"contacts"`
}

func (h *handlers) searchContacts(ctx context.Context, _ *mcp.CallToolRequest, in searchContactsInput) (*mcp.CallToolResult, searchContactsOutput, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return nil, searchContactsOutput{}, errors.New("query is required")
	}
	limit, _, err := pageParams(in.Limit, 0)
	if err != nil {
		return nil, searchContactsOutput{}, err
	}
	contacts, err := h.Client.Contacts(ctx)
	if err != nil {
		return nil, searchContactsOutput{}, fmt.Errorf("load contacts: %w", err)
	}
	chats, _, err := h.scanChats(ctx, "", chatScanPages)
	if err != nil {
		return nil, searchContactsOutput{}, err
	}
	dir := newDirectory(contacts)
	for _, c := range chats {
		dir.addChat(c)
	}

	q := parseContactQuery(query)
	type ranked struct {
		person *person
		tier   int
		key    string
	}
	var matches []ranked
	for _, p := range dir.all() {
		if tier, ok := q.match(p.fields()); ok {
			matches = append(matches, ranked{person: p, tier: tier, key: fold(cmp.Or(p.displayName(), p.push))})
		}
	}
	slices.SortFunc(matches, func(a, b ranked) int {
		return cmp.Or(cmp.Compare(a.tier, b.tier), cmp.Compare(a.key, b.key), cmp.Compare(a.person.jid, b.person.jid))
	})
	out := searchContactsOutput{Contacts: make([]contactOut, 0, min(len(matches), limit))}
	for _, m := range matches[:min(len(matches), limit)] {
		out.Contacts = append(out.Contacts, m.person.out())
	}
	return nil, out, nil
}

// contactDirectory loads the address book for name rendering. A client that
// cannot list contacts (e.g. not paired) yields an empty directory.
func (h *handlers) contactDirectory(ctx context.Context) *directory {
	contacts, err := h.Client.Contacts(ctx)
	if err != nil {
		h.Log.Debug("contacts unavailable for name rendering", "error", redact.Scrub(err.Error()))
		return newDirectory(nil)
	}
	return newDirectory(contacts)
}

// person is one contact or direct chat, reachable by its phone-number and its
// LID JID.
type person struct {
	jid, pn, lid              string
	name, firstName, business string
	push, chatName            string
	merged                    bool
}

// displayName is the best name the account owner chose or saw for p.
func (p *person) displayName() string {
	return cmp.Or(p.name, p.firstName, p.business, p.chatName)
}

func (p *person) out() contactOut {
	return contactOut{
		JID:      p.jid,
		Name:     p.displayName(),
		PushName: p.push,
		Phone:    phoneOf(p.pn),
		PN:       p.pn,
		LID:      p.lid,
	}
}

func (p *person) fields() matchFields {
	return matchFields{
		names: []string{p.name, p.firstName, p.business, p.chatName},
		push:  p.push,
		pn:    phoneOf(p.pn),
		lid:   lidUser(p.lid),
	}
}

type directory struct {
	people []*person
	byJID  map[string]*person
}

func newDirectory(contacts []wa.Contact) *directory {
	d := &directory{byJID: map[string]*person{}}
	for _, c := range contacts {
		d.addContact(c)
	}
	return d
}

func (d *directory) lookup(jid string) *person {
	if d == nil || jid == "" {
		return nil
	}
	return d.byJID[jid]
}

func (d *directory) all() []*person {
	out := make([]*person, 0, len(d.people))
	for _, p := range d.people {
		if !p.merged {
			out = append(out, p)
		}
	}
	return out
}

// addContact merges c into the person already known under any of its JIDs,
// so a LID entry and a phone-number entry of the same person become one.
func (d *directory) addContact(c wa.Contact) {
	if c.JID == "" || wa.IsGroup(c.JID) {
		return
	}
	pn := cmp.Or(c.PN, userJIDOf(c.JID, wa.ServerUser))
	lid := cmp.Or(c.LID, userJIDOf(c.JID, wa.ServerLID))
	p := d.personFor(c.JID, pn, lid)
	p.pn = cmp.Or(p.pn, pn)
	p.lid = cmp.Or(p.lid, lid)
	p.name = cmp.Or(p.name, c.Name)
	p.firstName = cmp.Or(p.firstName, c.FirstName)
	p.business = cmp.Or(p.business, c.BusinessName)
	p.push = cmp.Or(p.push, c.PushName)
	d.index(p, c.JID)
}

func (d *directory) addChat(c store.ChatSummary) {
	if c.IsGroup || c.JID == "" || wa.IsGroup(c.JID) {
		return
	}
	p := d.personFor(c.JID, userJIDOf(c.JID, wa.ServerUser), userJIDOf(c.JID, wa.ServerLID))
	p.pn = cmp.Or(p.pn, userJIDOf(c.JID, wa.ServerUser))
	p.lid = cmp.Or(p.lid, userJIDOf(c.JID, wa.ServerLID))
	p.chatName = cmp.Or(p.chatName, c.Name)
	d.index(p, c.JID)
}

// personFor returns the person known under any of jids, merging persons that
// turn out to be the same, or a new one.
func (d *directory) personFor(jids ...string) *person {
	var found *person
	for _, j := range jids {
		p := d.byJID[j]
		switch {
		case p == nil || p == found:
		case found == nil:
			found = p
		default:
			d.merge(found, p)
		}
	}
	if found == nil {
		found = &person{}
		d.people = append(d.people, found)
	}
	return found
}

func (d *directory) merge(into, from *person) {
	into.pn = cmp.Or(into.pn, from.pn)
	into.lid = cmp.Or(into.lid, from.lid)
	into.name = cmp.Or(into.name, from.name)
	into.firstName = cmp.Or(into.firstName, from.firstName)
	into.business = cmp.Or(into.business, from.business)
	into.push = cmp.Or(into.push, from.push)
	into.chatName = cmp.Or(into.chatName, from.chatName)
	from.merged = true
	for j, p := range d.byJID {
		if p == from {
			d.byJID[j] = into
		}
	}
}

func (d *directory) index(p *person, extra string) {
	p.jid = cmp.Or(p.pn, p.lid, p.jid, extra)
	for _, j := range []string{p.pn, p.lid, extra} {
		if j != "" {
			d.byJID[j] = p
		}
	}
}

// chatName renders a direct chat title: the saved address-book name, then the
// stored chat name, the business and push names, then the phone number. The
// saved name comes first because stored names of direct chats were often
// filled from push names before the address book was synced.
func (d *directory) chatName(c store.ChatSummary) string {
	if c.IsGroup {
		return c.Name
	}
	p := d.lookup(c.JID)
	if p == nil {
		return cmp.Or(c.Name, formatPhone(c.JID))
	}
	return cmp.Or(p.name, p.firstName, c.Name, p.business, p.push, formatPhone(p.pn), formatPhone(c.JID))
}

// senderName renders who sent m: the saved address-book name, then the push
// name recorded with the message, then the contact's names and number.
func (d *directory) senderName(m wa.Message) string {
	if m.FromMe {
		return m.SenderName
	}
	p := d.lookup(m.SenderJID)
	if p == nil {
		return cmp.Or(m.SenderName, formatPhone(m.SenderJID))
	}
	return cmp.Or(p.name, p.firstName, m.SenderName, p.business, p.push, formatPhone(p.pn), formatPhone(m.SenderJID))
}

func userJIDOf(jid, server string) string {
	if wa.Server(jid) != server {
		return ""
	}
	return jid
}

func phoneOf(jid string) string {
	if wa.Server(jid) != wa.ServerUser {
		return ""
	}
	return wa.User(jid)
}

func lidUser(jid string) string {
	if wa.Server(jid) != wa.ServerLID {
		return ""
	}
	return wa.User(jid)
}

func formatPhone(jid string) string {
	if digits := phoneOf(jid); digits != "" {
		return "+" + digits
	}
	return ""
}

// Match tiers, best first.
const (
	tierExact = iota
	tierPrefix
	tierSubstring
	tierPushName
	tierNumber
)

type matchFields struct {
	names []string
	push  string
	pn    string
	lid   string
}

// contactQuery is a parsed search: folded words that must all match, or the
// digit variants of a query that is a phone number.
type contactQuery struct {
	words  []string
	phones []string
}

func parseContactQuery(q string) contactQuery {
	if digits := phoneDigits(q); digits != "" {
		return contactQuery{phones: phoneVariants(digits)}
	}
	return contactQuery{words: words(q)}
}

// phoneVariants also yields the number without its international ("00") or
// national ("0") prefix, so "06 77…" finds +33 6 77….
func phoneVariants(digits string) []string {
	out := []string{digits}
	var bare string
	switch {
	case strings.HasPrefix(digits, "00"):
		bare = digits[2:]
	case strings.HasPrefix(digits, "0"):
		bare = digits[1:]
	}
	if len(bare) >= minPhoneQuery {
		out = append(out, bare)
	}
	return out
}

func (q contactQuery) match(f matchFields) (int, bool) {
	if len(q.phones) > 0 {
		return tierNumber, numberMatches(q.phones, f)
	}
	if len(q.words) == 0 {
		return 0, false
	}
	best := -1
	var names []string
	for _, n := range f.names {
		nw := words(n)
		if len(nw) == 0 {
			continue
		}
		names = append(names, strings.Join(nw, " "))
		switch {
		case sameWords(q.words, nw):
			best = tierExact
		case best != tierExact && allPrefixOfSome(q.words, nw):
			best = tierPrefix
		}
	}
	if best >= 0 {
		return best, true
	}
	nameText := strings.Join(names, " | ")
	if allContained(q.words, nameText) {
		return tierSubstring, true
	}
	fullText := nameText + " | " + strings.Join(words(f.push), " ")
	if allContained(q.words, fullText) {
		return tierPushName, true
	}
	for _, w := range q.words {
		if strings.Contains(fullText, w) {
			continue
		}
		if !isDigits(w) || len(w) < minPhoneQuery || !numberMatches(phoneVariants(w), f) {
			return 0, false
		}
	}
	return tierNumber, true
}

// numberMatches checks the phone number anywhere and a LID only from its start:
// LIDs are long random numbers that short fragments would match by chance.
func numberMatches(variants []string, f matchFields) bool {
	for _, v := range variants {
		if (f.pn != "" && strings.Contains(f.pn, v)) || (f.lid != "" && strings.HasPrefix(f.lid, v)) {
			return true
		}
	}
	return false
}

func sameWords(query, name []string) bool {
	if len(query) != len(name) {
		return false
	}
	return slices.Equal(slices.Sorted(slices.Values(query)), slices.Sorted(slices.Values(name)))
}

func allPrefixOfSome(query, name []string) bool {
	for _, q := range query {
		if !slices.ContainsFunc(name, func(n string) bool { return strings.HasPrefix(n, q) }) {
			return false
		}
	}
	return true
}

func allContained(query []string, text string) bool {
	for _, q := range query {
		if !strings.Contains(text, q) {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// letterFolds covers letters that Unicode does not decompose into a base
// letter plus accents.
var letterFolds = strings.NewReplacer("ß", "ss", "æ", "ae", "œ", "oe", "ø", "o", "ł", "l", "đ", "d", "ı", "i")

// fold lowercases s and strips accents, so "Anaïs" and "anais" compare
// equal.
func fold(s string) string {
	decomposed := norm.NFD.String(strings.ToLower(s))
	var b strings.Builder
	b.Grow(len(decomposed))
	for _, r := range decomposed {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(r)
		}
	}
	return letterFolds.Replace(b.String())
}

func words(s string) []string {
	return strings.FieldsFunc(fold(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// phoneDigits returns the digits of q when q consists only of phone-number
// characters and holds enough digits to be a meaningful fragment.
func phoneDigits(q string) string {
	var b strings.Builder
	for _, r := range q {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' || r == ' ' || r == '-' || r == '.' || r == '(' || r == ')':
		default:
			return ""
		}
	}
	if b.Len() < minPhoneQuery {
		return ""
	}
	return b.String()
}
