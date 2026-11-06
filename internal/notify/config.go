// Package notify delivers optional, debounced inbound message webhooks.
package notify

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"

	"github.com/fmflurry/whatsapp-mcp/internal/securefile"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

// Config is private webhook configuration; it must never be logged or exposed.
type Config struct {
	Enabled             bool     `json:"enabled"`
	URL                 string   `json:"url"`
	AuthorizationHeader string   `json:"authorization_header"`
	AllowedSenders      []string `json:"allowed_senders"`
	AllowedGroups       []string `json:"allowed_groups"`
	DebounceSeconds     int      `json:"debounce_seconds"`
	TextPreviewChars    int      `json:"text_preview_chars"`
}

// Load validates a private JSON file and applies delivery defaults.
func Load(path string) (Config, error) {
	b, err := securefile.Read(path, false)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, errors.New("not found")
	}
	if err != nil {
		return Config{}, errors.New("config must be a readable regular file with mode 0600 or 0400")
	}
	var c Config
	if json.Unmarshal(b, &c) != nil {
		return Config{}, errors.New("invalid config JSON")
	}
	if c.DebounceSeconds == 0 {
		c.DebounceSeconds = 20
	}
	if c.TextPreviewChars == 0 {
		c.TextPreviewChars = 280
	}
	if c.DebounceSeconds < 1 || c.DebounceSeconds > 600 || c.TextPreviewChars < 0 || c.TextPreviewChars > 2000 {
		return Config{}, errors.New("config limits out of range")
	}
	h := strings.TrimSpace(c.AuthorizationHeader)
	if prefix, value, ok := strings.Cut(h, ":"); ok && strings.EqualFold(strings.TrimSpace(prefix), "Authorization") {
		h = strings.TrimSpace(value)
	}
	if strings.ContainsAny(h, "\r\n") {
		return Config{}, errors.New("invalid authorization header")
	}
	c.AuthorizationHeader = h
	if c.URL != "" {
		u, err := url.Parse(c.URL)
		if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"))) {
			return Config{}, errors.New("webhook URL must use HTTPS (HTTP allowed only on loopback)")
		}
	} else if c.Enabled {
		return Config{}, errors.New("enabled webhook requires URL")
	}
	for _, s := range c.AllowedSenders {
		if _, ok := digits(s); !ok {
			return Config{}, errors.New("invalid allowed sender")
		}
	}
	for _, s := range c.AllowedGroups {
		if !wa.IsGroup(s) || wa.User(s) == "" {
			return Config{}, errors.New("invalid allowed group")
		}
	}
	return c, nil
}

func digits(s string) (string, bool) {
	if strings.Contains(s, "@") {
		if wa.Server(s) != wa.ServerUser && wa.Server(s) != wa.ServerLID {
			return "", false
		}
		user := wa.User(s)
		if user == "" {
			return "", false
		}
		for _, r := range user {
			if r < '0' || r > '9' {
				return "", false
			}
		}
		return user, true
	}
	return wa.NormalizePhone(s)
}

func (c Config) allowed(m wa.Message, pn func(string) string) bool {
	if wa.IsGroup(m.ChatJID) {
		for _, g := range c.AllowedGroups {
			if g == m.ChatJID {
				return true
			}
		}
		return false
	}
	if wa.Server(m.ChatJID) != wa.ServerUser && wa.Server(m.ChatJID) != wa.ServerLID {
		return false
	}
	if len(c.AllowedSenders) == 0 {
		return true
	}

	for _, jid := range []string{m.ChatJID, m.SenderJID} {
		candidates := []string{jid}
		if wa.Server(jid) == wa.ServerLID && pn != nil {
			candidates = append(candidates, pn(jid))
		}
		for _, candidate := range candidates {
			d, ok := digits(candidate)
			if !ok {
				continue
			}
			for _, s := range c.AllowedSenders {
				a, _ := digits(s)
				if a == d {
					return true
				}
				if wa.Server(s) == wa.ServerLID && pn != nil {
					a, _ = digits(pn(s))
					if a == d {
						return true
					}
				}
			}
		}
	}

	return false
}
