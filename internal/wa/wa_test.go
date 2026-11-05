package wa_test

import (
	"strings"
	"testing"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

func TestNormalizePhone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"plus prefix with spaces", "+33 6 12 34 56 78", "33612345678", true},
		{"double zero prefix", "0033612345678", "33612345678", true},
		{"double zero prefix with spaces", "00 33 6 12 34 56 78", "33612345678", true},
		{"dashes", "+33-6-12-34-56-78", "33612345678", true},
		{"dots", "+33.6.12.34.56.78", "33612345678", true},
		{"parentheses around area code", "+1 (415) 555-2671", "14155552671", true},
		{"plus inside parentheses", "(+33) 6 12 34 56 78", "33612345678", true},
		{"plus after opening parenthesis and space", "( +33 ) 6 12 34 56 78", "33612345678", true},
		{"bare digits", "33612345678", "33612345678", true},
		{"surrounding whitespace", "  +33612345678\t\n", "33612345678", true},
		{"minimum 7 digits", "1234567", "1234567", true},
		{"maximum 15 digits", "123456789012345", "123456789012345", true},
		{"prefix 00 then minimum digits", "001234567", "1234567", true},
		{"too short 6 digits", "123456", "", false},
		{"too short after 00 prefix", "0012345", "", false},
		{"too long 16 digits", "1234567890123456", "", false},
		{"too long after 00 prefix", "00" + "1234567890123456", "", false},
		{"empty", "", "", false},
		{"blank", "   ", "", false},
		{"plus only", "+", "", false},
		{"double zero only", "00", "", false},
		{"letters", "33612abc78", "", false},
		{"vanity number", "1-800-FLOWERS", "", false},
		{"plus in the middle", "33+612345678", "", false},
		{"second plus", "++33612345678", "", false},
		{"plus after a digit inside parentheses", "(3+3) 612345678", "", false},
		{"plus after a leading plus and parenthesis", "+(+33) 612345678", "", false},
		{"at sign", "33612345678@s.whatsapp.net", "", false},
		{"slash", "336/12345678", "", false},
		{"non ascii digits", "٣٣٦١٢٣٤٥٦٧٨", "", false},
		{"emoji", "+33612345678📞", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := wa.NormalizePhone(tt.in)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("NormalizePhone(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestNormalizePhoneOutputIsOnlyDigits(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"+33 (6) 12-34.56 78", "0033 612 345 678", "+1 415 555 2671"} {
		got, ok := wa.NormalizePhone(in)
		if !ok {
			t.Fatalf("NormalizePhone(%q) rejected", in)
		}
		if strings.Trim(got, "0123456789") != "" {
			t.Errorf("NormalizePhone(%q) = %q, want digits only", in, got)
		}
	}
}

func TestValidJID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		jid  string
		want bool
	}{
		{"33612345678@s.whatsapp.net", true},
		{"33612345678:12@s.whatsapp.net", true},
		{"123456789-1600000000@g.us", true},
		{"1234567890@lid", true},
		{"status@broadcast", true},
		{"120363000000000000@newsletter", true},
		{"", false},
		{"33612345678", false},
		{"@s.whatsapp.net", false},
		{"33612345678@", false},
		{"@", false},
		{"33612345678@example.com", false},
		{"33612345678@S.WHATSAPP.NET", false},
		{"a@b@g.us", false},
	}
	for _, tt := range tests {
		t.Run(tt.jid, func(t *testing.T) {
			t.Parallel()
			if got := wa.ValidJID(tt.jid); got != tt.want {
				t.Errorf("ValidJID(%q) = %v, want %v", tt.jid, got, tt.want)
			}
		})
	}
}

func TestServer(t *testing.T) {
	t.Parallel()
	tests := []struct{ jid, want string }{
		{"33612345678@s.whatsapp.net", "s.whatsapp.net"},
		{"123-456@g.us", "g.us"},
		{"33612345678:12@s.whatsapp.net", "s.whatsapp.net"},
		{"a@b@c", "b@c"},
		{"33612345678", ""},
		{"", ""},
		{"user@", ""},
	}
	for _, tt := range tests {
		if got := wa.Server(tt.jid); got != tt.want {
			t.Errorf("Server(%q) = %q, want %q", tt.jid, got, tt.want)
		}
	}
}

func TestUser(t *testing.T) {
	t.Parallel()
	tests := []struct{ jid, want string }{
		{"33612345678@s.whatsapp.net", "33612345678"},
		{"33612345678:12@s.whatsapp.net", "33612345678"},
		{"33612345678.0:12@s.whatsapp.net", "33612345678"},
		{"33612345678.1@lid", "33612345678"},
		{"123456789-1600000000@g.us", "123456789-1600000000"},
		{"33612345678", "33612345678"},
		{"33612345678:5", "33612345678"},
		{"@s.whatsapp.net", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := wa.User(tt.jid); got != tt.want {
			t.Errorf("User(%q) = %q, want %q", tt.jid, got, tt.want)
		}
	}
}

func TestIsGroup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		jid  string
		want bool
	}{
		{"123456789-1600000000@g.us", true},
		{"120363000000000000@g.us", true},
		{"33612345678@s.whatsapp.net", false},
		{"1234567890@lid", false},
		{"g.us", false},
		{"x@g.us.evil.com", false},
		{"x@notg.us", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := wa.IsGroup(tt.jid); got != tt.want {
			t.Errorf("IsGroup(%q) = %v, want %v", tt.jid, got, tt.want)
		}
	}
}

func TestUserJIDRoundTrip(t *testing.T) {
	t.Parallel()
	digits, ok := wa.NormalizePhone("+33 6 12 34 56 78")
	if !ok {
		t.Fatal("NormalizePhone rejected a valid number")
	}
	jid := wa.UserJID(digits)
	if jid != "33612345678@s.whatsapp.net" {
		t.Fatalf("UserJID = %q", jid)
	}
	if !wa.ValidJID(jid) {
		t.Errorf("UserJID output %q is not a valid JID", jid)
	}
	if wa.IsGroup(jid) {
		t.Errorf("UserJID output %q must not be a group", jid)
	}
	if got := wa.User(jid); got != digits {
		t.Errorf("User(UserJID(%q)) = %q", digits, got)
	}
	if got := wa.Server(jid); got != wa.ServerUser {
		t.Errorf("Server(UserJID(...)) = %q, want %q", got, wa.ServerUser)
	}
}

func TestSentinelErrorsAreDistinctAndDescriptive(t *testing.T) {
	t.Parallel()
	if wa.ErrNotConnected == wa.ErrNotPaired {
		t.Fatal("ErrNotConnected and ErrNotPaired must be distinct")
	}
	if !strings.Contains(wa.ErrNotPaired.Error(), "QR") {
		t.Errorf("ErrNotPaired should tell the user to scan the QR code, got %q", wa.ErrNotPaired)
	}
}

func TestOutgoingMediaType(t *testing.T) {
	t.Parallel()
	tests := map[string]wa.MediaType{
		"image/jpeg":                  wa.MediaImage,
		"image/png":                   wa.MediaImage,
		"image/webp":                  wa.MediaImage,
		"IMAGE/JPEG":                  wa.MediaImage,
		" image/png ; charset=binary": wa.MediaImage,
		"image/gif":                   wa.MediaDocument,
		"image/svg+xml":               wa.MediaDocument,
		"video/mp4":                   wa.MediaVideo,
		"Video/QuickTime; x=y":        wa.MediaVideo,
		"audio/ogg; codecs=opus":      wa.MediaAudio,
		"audio/mpeg":                  wa.MediaAudio,
		"application/pdf":             wa.MediaDocument,
		"text/plain":                  wa.MediaDocument,
		"":                            wa.MediaDocument,
	}
	for mime, want := range tests {
		if got := wa.OutgoingMediaType(mime); got != want {
			t.Errorf("OutgoingMediaType(%q) = %q, want %q", mime, got, want)
		}
	}
}
