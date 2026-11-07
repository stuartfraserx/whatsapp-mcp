package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/fmflurry/whatsapp-mcp/internal/config"
	"github.com/fmflurry/whatsapp-mcp/internal/datadir"
	"github.com/fmflurry/whatsapp-mcp/internal/redact"
)

const generatedTokenBytes = 32

// resolveHTTPToken returns the bearer token for the HTTP transport: the
// configured one, else the token persisted in the data dir (created on first
// use on loopback only). HTTP authentication cannot be disabled.
func resolveHTTPToken(cfg config.Config, log *slog.Logger) (string, error) {
	if cfg.HTTPToken != "" {
		return cfg.HTTPToken, nil
	}
	if cfg.HTTPNoAuth {
		return "", errors.New("HTTP MCP requires bearer authentication; http-no-auth is no longer supported")
	}
	path := cfg.HTTPTokenPath()
	token, err := readTokenFile(path)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		if !config.IsLoopbackHTTPAddr(cfg.HTTPAddr) {
			return "", errors.New("non-loopback HTTP requires WHATSAPP_MCP_HTTP_TOKEN or a configured token file")
		}
		if token, err = generateToken(); err != nil {
			return "", err
		}
		if err := datadir.WriteFileAtomic(path, []byte(token+"\n")); err != nil {
			return "", fmt.Errorf("store HTTP bearer token: %w", err)
		}
	default:
		return "", err
	}
	log.Info("HTTP bearer token stored in " + path)
	return token, nil
}

func readTokenFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("HTTP token file %s must be a regular file", path)
	}
	if perm := info.Mode().Perm(); perm != datadir.FilePerm {
		return "", fmt.Errorf("HTTP token file %s has mode %04o; it must be 0600", path, perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read HTTP token file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("HTTP token file %s is empty; delete it to generate a new token", path)
	}
	return token, nil
}

func generateToken() (string, error) {
	b := make([]byte, generatedTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate HTTP bearer token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// scrubWriter redacts phone numbers line by line before forwarding to w.
type scrubWriter struct {
	w   io.Writer
	buf []byte
}

func (s *scrubWriter) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := s.buf[:i+1]
		if _, err := io.WriteString(s.w, redact.Scrub(string(line))); err != nil {
			return 0, err
		}
		s.buf = s.buf[i+1:]
	}
}

// Flush forwards an unterminated trailing line.
func (s *scrubWriter) Flush() error {
	if len(s.buf) == 0 {
		return nil
	}
	_, err := io.WriteString(s.w, redact.Scrub(string(s.buf)))
	s.buf = nil
	return err
}
