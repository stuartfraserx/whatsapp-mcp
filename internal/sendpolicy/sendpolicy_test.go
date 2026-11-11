package sendpolicy_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/ratelimit"
	"github.com/fmflurry/whatsapp-mcp/internal/sendpolicy"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func mustParse(t *testing.T, entries ...string) sendpolicy.Allowlist {
	t.Helper()
	a, err := sendpolicy.ParseAllowlist(entries)
	if err != nil {
		t.Fatalf("ParseAllowlist(%q): %v", entries, err)
	}
	return a
}

const (
	alice      = "33612345678@s.whatsapp.net"
	bob        = "447911123456@s.whatsapp.net"
	familyChat = "123456789-1600000000@g.us"
)

func TestAllowlistAllowsByPhoneInAnyFormat(t *testing.T) {
	t.Parallel()
	entries := []string{"+33 6 12 34 56 78", "0033612345678", "33612345678", "+33-6-12-34-56-78", "(+33) 6 12 34 56 78"}
	for _, entry := range entries {
		a, err := sendpolicy.ParseAllowlist([]string{entry})
		if err != nil {
			t.Errorf("ParseAllowlist(%q): %v", entry, err)
			continue
		}
		if a.Empty() {
			t.Errorf("allowlist from %q is empty", entry)
		}
		for _, jid := range []string{alice, "33612345678:12@s.whatsapp.net", "33612345678.0:3@s.whatsapp.net"} {
			if !a.Allows(jid) {
				t.Errorf("allowlist %q must allow %q", entry, jid)
			}
		}
		for _, jid := range []string{bob, "3361234567@s.whatsapp.net", "336123456789@s.whatsapp.net", familyChat} {
			if a.Allows(jid) {
				t.Errorf("allowlist %q must not allow %q", entry, jid)
			}
		}
	}
}

func TestAllowlistAllowsByUserJID(t *testing.T) {
	t.Parallel()
	a := mustParse(t, alice)
	if !a.Allows(alice) || !a.Allows("33612345678:7@s.whatsapp.net") {
		t.Error("a user JID entry must allow that user, including linked-device suffixes")
	}
	if a.Allows(bob) {
		t.Error("a user JID entry must not allow other users")
	}
}

func TestAllowlistGroupsMatchExactly(t *testing.T) {
	t.Parallel()
	a := mustParse(t, familyChat)
	if !a.Allows(familyChat) {
		t.Error("the listed group must be allowed")
	}
	for _, jid := range []string{
		"123456789-1600000001@g.us",
		"123456789@g.us",
		"123456789-1600000000:1@g.us",
		alice,
		"123456789-1600000000@s.whatsapp.net",
	} {
		if a.Allows(jid) {
			t.Errorf("group allowlist must not allow %q", jid)
		}
	}
}

func TestPhoneEntryDoesNotAllowAGroupWithTheSameNumber(t *testing.T) {
	t.Parallel()
	a := mustParse(t, "33612345678")
	if a.Allows("33612345678@g.us") {
		t.Error("a phone entry must not allow a group whose ID equals the number")
	}
	g := mustParse(t, "33612345678@g.us")
	if g.Allows(alice) {
		t.Error("a group entry must not allow a user whose number equals the group ID")
	}
}

func TestMixedAllowlist(t *testing.T) {
	t.Parallel()
	a := mustParse(t, "+33 6 12 34 56 78", familyChat, "  ", bob)
	for _, jid := range []string{alice, familyChat, bob} {
		if !a.Allows(jid) {
			t.Errorf("must allow %q", jid)
		}
	}
	if a.Allows("491701234567@s.whatsapp.net") {
		t.Error("must not allow an unlisted user")
	}
}

func TestEmptyAllowlistAllowsEveryone(t *testing.T) {
	t.Parallel()
	cases := map[string]sendpolicy.Allowlist{
		"zero value":      {},
		"nil entries":     mustParse(t),
		"blank entries":   mustParse(t, "", "   ", "\t"),
		"empty (non-nil)": mustParse(t, []string{}...),
	}
	for name, a := range cases {
		if !a.Empty() {
			t.Errorf("%s: Empty() = false", name)
		}
		for _, jid := range []string{alice, familyChat, "x@lid", ""} {
			if !a.Allows(jid) {
				t.Errorf("%s: an empty allowlist must allow %q", name, jid)
			}
		}
	}
	if mustParse(t, alice).Empty() {
		t.Error("a populated allowlist must not be Empty")
	}
}

func TestParseAllowlistRejectsInvalidEntries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		entry string
	}{
		{"letters", "alice"},
		{"too short phone", "12345"},
		{"too long phone", "1234567890123456"},
		{"unknown server", "33612345678@example.com"},
		{"empty user jid", "@s.whatsapp.net"},
		{"empty server", "33612345678@"},
		{"phone with letters", "+33 6 12 ab 56 78"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, err := sendpolicy.ParseAllowlist([]string{alice, tt.entry})
			if err == nil {
				t.Fatalf("ParseAllowlist accepted %q", tt.entry)
			}
			if !a.Empty() {
				t.Error("a failed parse must not return a partial allowlist that restricts recipients")
			}
			if !strings.Contains(err.Error(), tt.entry) {
				t.Errorf("error %q should name the offending entry %q", err, tt.entry)
			}
		})
	}
}

func newPolicy(t *testing.T, clock ratelimit.Clock, interval time.Duration, hourly int, entries ...string) *sendpolicy.Policy {
	t.Helper()
	return &sendpolicy.Policy{
		Enabled:   true,
		Allowlist: mustParse(t, entries...),
		Limiter:   ratelimit.New(interval, hourly, clock),
	}
}

func TestCheckDisabledReturnsErrSendDisabled(t *testing.T) {
	t.Parallel()
	p := newPolicy(t, newClock(), 3*time.Second, 10)
	p.Enabled = false
	for _, jid := range []string{alice, familyChat, ""} {
		if err := p.Check(jid); !errors.Is(err, sendpolicy.ErrSendDisabled) {
			t.Errorf("Check(%q) = %v, want ErrSendDisabled", jid, err)
		}
	}
}

func TestCheckDisabledTakesPrecedenceOverAllowlistAndConsumesNoSlot(t *testing.T) {
	t.Parallel()
	clock := newClock()
	p := newPolicy(t, clock, time.Hour, 1, bob)
	p.Enabled = false
	for range 3 {
		if err := p.Check(alice); !errors.Is(err, sendpolicy.ErrSendDisabled) {
			t.Fatalf("Check = %v, want ErrSendDisabled", err)
		}
	}
	p.Enabled = true
	if err := p.Check(bob); err != nil {
		t.Errorf("Check after enabling = %v; refused sends must not have consumed the only slot", err)
	}
}

func TestCheckRefusedAllowlistDoesNotConsumeARateSlot(t *testing.T) {
	t.Parallel()
	clock := newClock()
	p := newPolicy(t, clock, time.Hour, 1, alice)

	for range 5 {
		if err := p.Check(bob); !errors.Is(err, sendpolicy.ErrNotAllowlisted) {
			t.Fatalf("Check(bob) = %v, want ErrNotAllowlisted", err)
		}
	}
	if err := p.Check(familyChat); !errors.Is(err, sendpolicy.ErrNotAllowlisted) {
		t.Fatalf("Check(group) = %v, want ErrNotAllowlisted", err)
	}
	if err := p.Check(alice); err != nil {
		t.Fatalf("Check(alice) = %v; the single slot must still be available after refusals", err)
	}
	if err := p.Check(alice); !errors.Is(err, sendpolicy.ErrRateLimited) {
		t.Fatalf("second Check(alice) = %v, want ErrRateLimited", err)
	}
}

func TestCheckConsumesOneSlotPerAllowedSend(t *testing.T) {
	t.Parallel()
	clock := newClock()
	p := newPolicy(t, clock, time.Second, 3)
	for i := range 3 {
		if err := p.Check(alice); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		clock.Advance(time.Second)
	}
	err := p.Check(alice)
	if !errors.Is(err, sendpolicy.ErrRateLimited) {
		t.Fatalf("fourth send = %v, want ErrRateLimited", err)
	}
	if errors.Is(err, sendpolicy.ErrNotAllowlisted) || errors.Is(err, sendpolicy.ErrSendDisabled) {
		t.Errorf("rate limit error must be distinct: %v", err)
	}
}

func TestCheckRateLimitedMessageRoundsRetryUpToWholeSeconds(t *testing.T) {
	t.Parallel()
	clock := newClock()
	p := newPolicy(t, clock, 3*time.Second, 100)
	if err := p.Check(alice); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		advance time.Duration
		want    string
	}{
		{0, "retry after 3s"},
		{500 * time.Millisecond, "retry after 3s"},
		{500 * time.Millisecond, "retry after 2s"},
		{1500 * time.Millisecond, "retry after 1s"},
		{400 * time.Millisecond, "retry after 1s"},
	}
	for _, tt := range tests {
		clock.Advance(tt.advance)
		err := p.Check(alice)
		if !errors.Is(err, sendpolicy.ErrRateLimited) {
			t.Fatalf("Check = %v, want ErrRateLimited", err)
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("error %q does not contain %q", err, tt.want)
		}
	}
}

func TestCheckRateLimitedByHourlyCapReportsLongRetry(t *testing.T) {
	t.Parallel()
	clock := newClock()
	p := newPolicy(t, clock, time.Second, 1)
	if err := p.Check(alice); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	err := p.Check(alice)
	if !errors.Is(err, sendpolicy.ErrRateLimited) {
		t.Fatalf("Check = %v", err)
	}
	if !strings.Contains(err.Error(), "retry after 3599s") {
		t.Errorf("error %q should report 3599s", err)
	}
}

func TestCheckAfterWaitingSucceedsAgain(t *testing.T) {
	t.Parallel()
	clock := newClock()
	p := newPolicy(t, clock, 3*time.Second, 100)
	if err := p.Check(alice); err != nil {
		t.Fatal(err)
	}
	if err := p.Check(alice); !errors.Is(err, sendpolicy.ErrRateLimited) {
		t.Fatalf("immediate retry = %v", err)
	}
	clock.Advance(3 * time.Second)
	if err := p.Check(alice); err != nil {
		t.Errorf("Check after the interval = %v", err)
	}
}

func TestCheckWithNilLimiterOnlyAppliesEnabledAndAllowlist(t *testing.T) {
	t.Parallel()
	p := &sendpolicy.Policy{Enabled: true, Allowlist: mustParse(t, alice)}
	for range 5 {
		if err := p.Check(alice); err != nil {
			t.Fatalf("Check(alice) = %v", err)
		}
	}
	if err := p.Check(bob); !errors.Is(err, sendpolicy.ErrNotAllowlisted) {
		t.Errorf("Check(bob) = %v, want ErrNotAllowlisted", err)
	}
}

func TestCheckEmptyAllowlistAllowsAnyRecipientSubjectToRate(t *testing.T) {
	t.Parallel()
	clock := newClock()
	p := newPolicy(t, clock, time.Second, 2)
	if err := p.Check(alice); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	if err := p.Check(familyChat); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	if err := p.Check(bob); !errors.Is(err, sendpolicy.ErrRateLimited) {
		t.Errorf("Check = %v, want ErrRateLimited", err)
	}
}

func TestSentinelErrorsAreDistinct(t *testing.T) {
	t.Parallel()
	errs := []error{sendpolicy.ErrSendDisabled, sendpolicy.ErrNotAllowlisted, sendpolicy.ErrRateLimited}
	for i, a := range errs {
		for j, b := range errs {
			if (i == j) != errors.Is(a, b) {
				t.Errorf("errors.Is(%v, %v) = %v", a, b, errors.Is(a, b))
			}
		}
	}
}

func TestConcurrentChecksNeverExceedTheCap(t *testing.T) {
	t.Parallel()
	const hourly = 20
	p := newPolicy(t, newClock(), time.Second, hourly)
	p.Limiter = ratelimit.New(0, hourly, newClock())

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		allowed int
	)
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Check(alice); err == nil {
				mu.Lock()
				allowed++
				mu.Unlock()
			} else if !errors.Is(err, sendpolicy.ErrRateLimited) {
				t.Errorf("unexpected error %v", err)
			}
		}()
	}
	wg.Wait()
	if allowed != hourly {
		t.Errorf("%d sends allowed, want exactly %d", allowed, hourly)
	}
}

func TestAllowlistIsKeyedByServerAndUser(t *testing.T) {
	t.Parallel()
	a := mustParse(t, "+33612345678", "999@lid")
	for _, jid := range []string{"33612345678@lid", "33612345678@broadcast", "33612345678@newsletter", "999@s.whatsapp.net"} {
		if a.Allows(jid) {
			t.Errorf("must not allow %q: same user on a different server", jid)
		}
	}
	for _, jid := range []string{alice, "33612345678:4@s.whatsapp.net", "999@lid", "999:2@lid"} {
		if !a.Allows(jid) {
			t.Errorf("must allow %q", jid)
		}
	}
}

func TestAllowlistJIDEntryWithDeviceSuffixIsCanonicalized(t *testing.T) {
	t.Parallel()
	a := mustParse(t, "33612345678:9@s.whatsapp.net")
	if !a.Allows(alice) || !a.Allows("33612345678.0:1@s.whatsapp.net") {
		t.Error("an entry with a device suffix must allow the plain user JID and other devices")
	}
}

func TestRefundGivesTheSlotBack(t *testing.T) {
	t.Parallel()
	clock := newClock()
	p := newPolicy(t, clock, 3*time.Second, 1)
	slot, err := p.CheckSlot(alice)
	if err != nil {
		t.Fatalf("first CheckSlot: %v", err)
	}
	if err := p.Check(alice); !errors.Is(err, sendpolicy.ErrRateLimited) {
		t.Fatalf("second Check = %v, want ErrRateLimited", err)
	}
	p.Refund(slot)
	if err := p.Check(alice); err != nil {
		t.Fatalf("Check after Refund: %v", err)
	}
}

func TestRefundOnlyGivesBackTheCallersOwnSlot(t *testing.T) {
	t.Parallel()
	clock := newClock()
	p := newPolicy(t, clock, 10*time.Second, 10)
	slotA, err := p.CheckSlot(alice)
	if err != nil {
		t.Fatalf("A: %v", err)
	}
	clock.Advance(10 * time.Second)
	if _, err := p.CheckSlot(alice); err != nil {
		t.Fatalf("B: %v", err)
	}

	p.Refund(slotA)

	clock.Advance(time.Second)
	err = p.Check(alice)
	if !errors.Is(err, sendpolicy.ErrRateLimited) || !strings.Contains(err.Error(), "retry after 9s") {
		t.Fatalf("C = %v, want ErrRateLimited with retry after 9s", err)
	}
}

func TestRefundIsNilSafe(t *testing.T) {
	t.Parallel()
	var nilPolicy *sendpolicy.Policy
	nilPolicy.Refund(ratelimit.Slot{})
	(&sendpolicy.Policy{Enabled: true}).Refund(ratelimit.Slot{})
}

func TestStatusSlotSkipsOnlyChatAllowlist(t *testing.T) {
	p := &sendpolicy.Policy{Enabled: true, Allowlist: mustParse(t, alice), Limiter: ratelimit.New(time.Second, 10, newClock())}
	if _, err := p.CheckSlot("status@broadcast"); !errors.Is(err, sendpolicy.ErrNotAllowlisted) {
		t.Fatalf("chat path err=%v", err)
	}
	slot, err := p.CheckStatusSlot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.CheckSlot(alice); !errors.Is(err, sendpolicy.ErrRateLimited) {
		t.Fatalf("shared rate limit err=%v", err)
	}
	p.Refund(slot)
	if _, err = p.CheckStatusSlot(); err != nil {
		t.Fatalf("refund err=%v", err)
	}
	p.Enabled = false
	if _, err = p.CheckStatusSlot(); !errors.Is(err, sendpolicy.ErrSendDisabled) {
		t.Fatalf("disabled err=%v", err)
	}
}
