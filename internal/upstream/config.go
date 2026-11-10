// Package upstream loads private configuration for remote stdio proxies.
package upstream

import (
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fmflurry/whatsapp-mcp/internal/securefile"
)

const EnvURL = "WHATSAPP_MCP_UPSTREAM_URL"
const EnvTokenFile = "WHATSAPP_MCP_UPSTREAM_TOKEN_FILE"

type Config struct {
	URL       string `json:"url"`
	TokenFile string `json:"token_file"`
}

// Load returns nil only when neither environment nor file configuration exists.
func Load(dataDir string, lookup func(string) (string, bool)) (*Config, error) {
	rawURL, hasURL := lookup(EnvURL)
	tokenFile, hasToken := lookup(EnvTokenFile)
	c := &Config{URL: rawURL, TokenFile: tokenFile}
	if !hasURL && !hasToken {
		b, err := securefile.Read(filepath.Join(dataDir, "upstream.json"), false)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, errors.New("upstream.json must be a readable regular private file (0600 or 0400), without symlinks")
		}
		if json.Unmarshal(b, c) != nil {
			return nil, errors.New("invalid upstream.json JSON")
		}
	}
	if err := ValidateURL(c.URL); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(c.TokenFile) {
		return nil, errors.New("upstream token_file must be absolute")
	}
	if _, err := c.Token(); err != nil {
		return nil, err
	}
	return c, nil
}

func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return errors.New("invalid upstream URL: require a host, no userinfo or fragment")
	}
	if strings.HasSuffix(u.Host, ":") {
		return errors.New("invalid upstream URL port")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("invalid upstream URL port")
		}
	}
	if u.Scheme == "https" {
		return nil
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme == "http" && ip != nil && ip.IsLoopback() {
		return nil
	}
	return errors.New("upstream URL must use HTTPS (HTTP requires a literal loopback address)")
}

// Token reads credentials anew for each connection, without exposing file contents.
func (c Config) Token() (string, error) {
	b, err := securefile.Read(c.TokenFile, false)
	if err != nil {
		return "", errors.New("upstream token_file must be a readable regular private file (0600 or 0400), without symlinks")
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", errors.New("upstream token_file is empty")
	}
	return token, nil
}
