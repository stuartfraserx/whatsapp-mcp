package redact_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/fmflurry/whatsapp-mcp/internal/redact"
)

func TestJID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"user jid", "33612345678@s.whatsapp.net", "***78@s.whatsapp.net"},
		{"group jid", "123456789-1600000000@g.us", "***00@g.us"},
		{"device suffix does not leak", "33612345678:12@s.whatsapp.net", "***12@s.whatsapp.net"},
		{"empty", "", ""},
		{"two character user", "ab@lid", "***@lid"},
		{"one character user", "a@lid", "***@lid"},
		{"empty user", "@g.us", "***@g.us"},
		{"three character user", "abc@lid", "***bc@lid"},
		{"no server", "33612345678", "***78"},
		{"unicode user keeps two runes", "日本語テキスト@lid", "***スト@lid"},
		{"only first at sign splits", "a1234@b@c", "***34@b@c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := redact.JID(tt.in); got != tt.want {
				t.Errorf("JID(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestJIDNeverLeaksMoreThanTwoUserCharacters(t *testing.T) {
	t.Parallel()
	user := "33612345678"
	got := redact.JID(user + "@s.whatsapp.net")
	for i := 3; i <= len(user); i++ {
		if strings.Contains(got, user[:i]) || strings.Contains(got, user[len(user)-i:]) {
			t.Fatalf("JID output %q leaks %d characters of the user part", got, i)
		}
	}
}

func TestScrub(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"international number", "call +33612345678 now", "call [redacted] now"},
		{"bare number", "call 33612345678 now", "call [redacted] now"},
		{"exactly six digits", "123456", "[redacted]"},
		{"five digits kept", "12345", "12345"},
		{"plus with five digits kept", "+12345", "+12345"},
		{"plus with six digits", "+123456", "[redacted]"},
		{"double plus keeps one", "++33612345678", "+[redacted]"},
		{"digits glued to letters", "a1234567b", "a[redacted]b"},
		{"jid user part", "33612345678@s.whatsapp.net", "[redacted]@s.whatsapp.net"},
		{"two numbers", "from 33612345678 to 447911123456", "from [redacted] to [redacted]"},
		{"separated groups below threshold", "id 12345-67890", "id 12345-67890"},
		{"spaced number is not joined", "33 6 12 34", "33 6 12 34"},
		{"empty", "", ""},
		{"no digits", "hello world", "hello world"},
		{"timestamps shorter than six", "12:30:45", "12:30:45"},
		{"long digit run is one match", "1234567890123456789", "[redacted]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := redact.Scrub(tt.in); got != tt.want {
				t.Errorf("Scrub(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestScrubLeavesNoLongDigitRunAndIsIdempotent(t *testing.T) {
	t.Parallel()
	longRun := regexp.MustCompile(`[0-9]{6,}`)
	inputs := []string{
		"error dialing 33612345678@s.whatsapp.net: timeout",
		"+1 415 5552671 and 4155552671 and 0033612345678",
		"msg 120363000000000000@g.us from 33612345678:12@s.whatsapp.net",
		strings.Repeat("9", 40),
	}
	for _, in := range inputs {
		once := redact.Scrub(in)
		if longRun.MatchString(once) {
			t.Errorf("Scrub(%q) = %q still contains a digit run of six or more", in, once)
		}
		if twice := redact.Scrub(once); twice != once {
			t.Errorf("Scrub is not idempotent: %q then %q", once, twice)
		}
	}
}
