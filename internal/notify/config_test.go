package notify

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "webhook.json")
	if _, err := Load(path); err == nil || err.Error() != "not found" {
		t.Fatalf("missing: %v", err)
	}
	writeConfig(t, path, `{"enabled":true,"url":"https://example.test/wake?secret=value","authorization_header":"  Authorization: Bearer secret  "}`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.AuthorizationHeader != "Bearer secret" || c.DebounceSeconds != 20 || c.TextPreviewChars != 280 || c.CatchupMinutes != 60 {
		t.Fatalf("bad defaults/header: %+v", c)
	}
	for _, mode := range []os.FileMode{0640, 0604, 0700, 0200} {
		os.Chmod(path, mode)
		if _, err := Load(path); err == nil {
			t.Errorf("accepted mode %o", mode)
		}
	}
	os.Chmod(path, 0600)
	link := path + "-link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(link); err == nil {
		t.Fatal("accepted symlink")
	}
	if _, err := Load(filepath.Dir(path)); err == nil {
		t.Fatal("accepted directory")
	}
	for _, body := range []string{`{`, `{"enabled":true}`, `{"url":"http://example.test/x"}`, `{"url":"https://user:pass@example.test"}`, `{"catchup_minutes":-1}`, `{"catchup_minutes":1441}`, `{"debounce_seconds":601}`, `{"debounce_seconds":-1}`, `{"text_preview_chars":2001}`, `{"allowed_groups":["123@s.whatsapp.net"]}`} {
		writeConfig(t, path, body)
		if _, err := Load(path); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	for _, minutes := range []int{0, 60, 1440} {
		writeConfig(t, path, fmt.Sprintf(`{"catchup_minutes":%d}`, minutes))
		c, err := Load(path)
		if err != nil || c.CatchupMinutes != minutes {
			t.Fatalf("catchup %d: got %d, error %v", minutes, c.CatchupMinutes, err)
		}
	}
	for _, host := range []string{"127.0.0.1", "localhost"} {
		writeConfig(t, path, `{"enabled":true,"url":"http://`+host+`:1234/x"}`)
		if _, err := Load(path); err != nil {
			t.Fatal(err)
		}
	}
}
func TestAllowed(t *testing.T) {
	m := wa.Message{ChatJID: "573133992340@s.whatsapp.net", SenderJID: "573133992340@s.whatsapp.net"}
	for _, s := range []string{"573133992340", "+57 313 399 2340", "0057 (313) 399-2340", "573133992340@s.whatsapp.net"} {
		c := Config{AllowedSenders: []string{s}}
		if !c.allowed(m, nil) {
			t.Errorf("failed %s", s)
		}
		lid := m
		lid.ChatJID = "123456789012345@lid"
		lid.SenderJID = lid.ChatJID
		if c.allowed(lid, nil) {
			t.Fatal("unexpected unmapped lid match")
		}
		if !c.allowed(lid, func(string) string { return m.ChatJID }) {
			t.Fatal("mapped lid did not match")
		}
	}
	if !(Config{}).allowed(m, nil) {
		t.Fatal("empty direct allowlist")
	}
	m.ChatJID = "12345@g.us"
	if (Config{}).allowed(m, nil) {
		t.Fatal("empty group list allowed group")
	}
	if !(Config{AllowedGroups: []string{m.ChatJID}, AllowedSenders: []string{"9999999"}}).allowed(m, nil) {
		t.Fatal("group sender restriction")
	}
	if (Config{AllowedGroups: []string{strings.ReplaceAll(m.ChatJID, "12345", "98765")}}).allowed(m, nil) {
		t.Fatal("wrong group")
	}
}
