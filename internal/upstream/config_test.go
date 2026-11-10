package upstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	for _, mode := range []os.FileMode{0600, 0400} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			token := filepath.Join(dir, "token")
			if err := os.WriteFile(token, []byte(" secret\n"), mode); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(Config{URL: "https://example.com/mcp", TokenFile: token})
			if err := os.WriteFile(filepath.Join(dir, "upstream.json"), b, mode); err != nil {
				t.Fatal(err)
			}
			c, err := Load(dir, func(string) (string, bool) { return "", false })
			if err != nil {
				t.Fatal(err)
			}
			if got, err := c.Token(); got != "secret" || err != nil {
				t.Fatalf("token read failed: %v", err)
			}
			// Environment takes precedence even over an invalid file.
			if err := os.Chmod(filepath.Join(dir, "upstream.json"), 0644); err != nil {
				t.Fatal(err)
			}
			values := map[string]string{EnvURL: "https://other.example/mcp", EnvTokenFile: token}
			c, err = Load(dir, func(k string) (string, bool) { v, ok := values[k]; return v, ok })
			if err != nil || c.URL != values[EnvURL] {
				t.Fatalf("env precedence: %v", err)
			}
			delete(values, EnvTokenFile)
			if _, err = Load(dir, func(k string) (string, bool) { v, ok := values[k]; return v, ok }); err == nil {
				t.Fatal("partial env accepted")
			}
		})
	}
}

func TestURL(t *testing.T) {
	for _, tt := range []struct {
		url string
		ok  bool
	}{
		{"https://example.com/mcp", true}, {"http://127.0.0.1:8765/mcp", true}, {"http://[::1]/mcp", true},
		{"http://example.com/mcp", false}, {"http://localhost/mcp", false}, {"http://0.0.0.0/mcp", false}, {"ftp://example.com/mcp", false}, {"https:///mcp", false}, {"https://user:secret@example.com/mcp", false}, {"", false},
	} {
		t.Run(tt.url, func(t *testing.T) {
			if err := ValidateURL(tt.url); (err == nil) != tt.ok {
				t.Fatalf("validation: %v", err)
			}
		})
	}
}

func TestUnsafeFiles(t *testing.T) {
	for _, target := range []string{"upstream.json", "token"} {
		for _, kind := range []string{"permissions", "symlink", "directory", "empty", "invalid"} {
			t.Run(target+"/"+kind, func(t *testing.T) {
				dir := t.TempDir()
				token := filepath.Join(dir, "token")
				path := filepath.Join(dir, target)
				os.WriteFile(token, []byte("sensitive-token"), 0600)
				b, _ := json.Marshal(Config{URL: "https://example.com/mcp", TokenFile: token})
				os.WriteFile(filepath.Join(dir, "upstream.json"), b, 0600)
				switch kind {
				case "permissions":
					os.Chmod(path, 0644)
				case "symlink":
					os.Rename(path, path+".real")
					os.Symlink(path+".real", path)
				case "directory":
					os.Remove(path)
					os.Mkdir(path, 0700)
				case "empty":
					os.WriteFile(path, nil, 0600)
				case "invalid":
					if target == "token" {
						return
					}
					os.WriteFile(path, []byte(`{"url": "sensitive-token"`), 0600)
				}
				_, err := Load(dir, func(string) (string, bool) { return "", false })
				if err == nil {
					t.Fatal("unsafe config accepted")
				}
				if strings.Contains(err.Error(), "sensitive-token") {
					t.Fatal("contents leaked")
				}
			})
		}
	}
}

func TestMissingAndRelativeToken(t *testing.T) {
	dir := t.TempDir()
	lookup := func(string) (string, bool) { return "", false }
	if c, err := Load(dir, lookup); c != nil || err != nil {
		t.Fatalf("missing: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "upstream.json"), []byte(`{"url":"https://example.com/mcp","token_file":"relative"}`), 0600)
	if _, err := Load(dir, lookup); err == nil {
		t.Fatal("relative token accepted")
	}
}
