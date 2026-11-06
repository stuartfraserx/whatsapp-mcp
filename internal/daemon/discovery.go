// Package daemon manages private daemon discovery and safe detached startup.
package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/config"
	"github.com/fmflurry/whatsapp-mcp/internal/datadir"
	"github.com/fmflurry/whatsapp-mcp/internal/securefile"
)

// Info locates a daemon without storing its bearer token.
type Info struct {
	HTTPAddr     string   `json:"http_addr"`
	TokenFile    string   `json:"token_file"`
	PID          int      `json:"pid"`
	StartedAt    string   `json:"started_at"`
	StartCommand []string `json:"start_command,omitempty"`
	LogPath      string   `json:"log_path,omitempty"`
}

// Path returns the discovery file in the data directory.
func Path(dir string) string { return filepath.Join(dir, "daemon.json") }

// Read accepts only private, regular discovery files and loopback endpoints.
func Read(dir string) (Info, error) {
	b, err := securefile.Read(Path(dir), true)
	if err != nil {
		return Info{}, err
	}
	var d Info
	if json.Unmarshal(b, &d) != nil {
		return Info{}, errors.New("invalid daemon discovery JSON")
	}
	if err := config.ValidateHTTPAddr(d.HTTPAddr); err != nil {
		return Info{}, errors.New("invalid daemon HTTP address")
	}
	if d.TokenFile != "" && !filepath.IsAbs(d.TokenFile) {
		return Info{}, errors.New("daemon token path must be absolute")
	}
	return d, nil
}

// Write atomically publishes private discovery metadata for this process.
func Write(dir string, d Info) error {
	d.PID = os.Getpid()
	d.StartedAt = time.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return datadir.WriteFileAtomic(Path(dir), b)
}
