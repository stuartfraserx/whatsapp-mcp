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
	lookup := func(k string) (string, bool) {
		if k == "HOME" {
			return dir, true
		}
		return "", false
	}
	if c, err := Load(dir, lookup); c != nil || err != nil {
		t.Fatalf("missing: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "upstream.json"), []byte(`{"url":"https://example.com/mcp","token_file":"relative"}`), 0600)
	if _, err := Load(dir, lookup); err == nil {
		t.Fatal("relative token accepted")
	}
}

func TestGlobalFallbackOrder(t *testing.T) {
	for _, source := range []string{"env", "data", "explicit", "xdg", "home", "os-home"} {
		t.Run(source, func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(dir, "home")
			t.Setenv("HOME", home)
			token := filepath.Join(dir, "token")
			if err := os.WriteFile(token, []byte("test-token"), 0600); err != nil {
				t.Fatal(err)
			}
			paths := map[string]string{
				"data":     filepath.Join(dir, "data", "upstream.json"),
				"explicit": filepath.Join(dir, "custom.json"),
				"xdg":      filepath.Join(dir, "xdg", "whatsapp-mcp", "upstream.json"),
				"home":     filepath.Join(home, ".config", "whatsapp-mcp", "upstream.json"),
			}
			for name, path := range paths {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				b, _ := json.Marshal(Config{URL: "https://" + name + ".example/mcp", TokenFile: token})
				if err := os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			values := map[string]string{EnvURL: "https://env.example/mcp", EnvTokenFile: token, EnvConfig: paths["explicit"], "XDG_CONFIG_HOME": filepath.Join(dir, "xdg"), "HOME": home}
			if source != "env" {
				delete(values, EnvURL)
				delete(values, EnvTokenFile)
			}
			if source != "env" && source != "data" {
				if err := os.Remove(paths["data"]); err != nil {
					t.Fatal(err)
				}
			}
			if source == "xdg" || source == "home" || source == "os-home" {
				delete(values, EnvConfig)
			}
			if source == "home" || source == "os-home" {
				delete(values, "XDG_CONFIG_HOME")
			}
			want := source
			if source == "os-home" {
				delete(values, "HOME")
				want = "home"
			}
			c, err := Load(filepath.Dir(paths["data"]), func(k string) (string, bool) { v, ok := values[k]; return v, ok })
			if err != nil || c == nil || c.URL != "https://"+want+".example/mcp" {
				t.Fatalf("source %s: config %v, error %v", source, c, err)
			}
		})
	}
}

func TestGlobalFiles(t *testing.T) {
	for _, source := range []string{"explicit", "xdg", "home"} {
		for _, kind := range []string{"missing", "0400", "permissions", "symlink", "dangling-symlink", "directory", "json", "url", "token"} {
			t.Run(source+"/"+kind, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "whatsapp-mcp", "upstream.json")
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				token := filepath.Join(dir, "token")
				if err := os.WriteFile(token, []byte("sensitive-token"), 0600); err != nil {
					t.Fatal(err)
				}
				c := Config{URL: "https://example.com/mcp", TokenFile: token}
				if kind == "url" {
					c.URL = "sensitive-token"
				}
				if kind == "token" {
					c.TokenFile = "sensitive-token"
				}
				b, _ := json.Marshal(c)
				if kind == "json" {
					b = []byte("sensitive-token")
				}
				if err := os.WriteFile(path, b, 0600); err != nil {
					t.Fatal(err)
				}
				var err error
				switch kind {
				case "missing":
					err = os.Remove(path)
				case "0400":
					err = os.Chmod(path, 0400)
				case "permissions":
					err = os.Chmod(path, 0644)
				case "symlink", "dangling-symlink":
					if err = os.Remove(path); err == nil {
						target := token
						if kind == "dangling-symlink" {
							target += "-missing"
						}
						err = os.Symlink(target, path)
					}
				case "directory":
					if err = os.Remove(path); err == nil {
						err = os.Mkdir(path, 0700)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				values := map[string]string{"HOME": filepath.Join(dir, "unused")}
				switch source {
				case "explicit":
					values[EnvConfig] = path
				case "xdg":
					values["XDG_CONFIG_HOME"] = dir
				case "home":
					values["HOME"] = dir
					if err := os.MkdirAll(filepath.Join(dir, ".config"), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(filepath.Dir(path), filepath.Join(dir, ".config", "whatsapp-mcp")); err != nil {
						t.Fatal(err)
					}
				}
				got, err := Load(filepath.Join(dir, "data"), func(k string) (string, bool) { v, ok := values[k]; return v, ok })
				if kind == "missing" {
					if got != nil || err != nil {
						t.Fatalf("missing: %v, %v", got, err)
					}
				} else if kind == "0400" {
					if got == nil || err != nil {
						t.Fatalf("private file: %v", err)
					}
				} else if err == nil || strings.Contains(err.Error(), "sensitive-token") {
					t.Fatalf("expected safe validation error, got %v", err)
				}
			})
		}
	}
}

func TestInvalidDataFileStopsGlobalFallback(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(dir, "global.json")
	b, _ := json.Marshal(Config{URL: "https://example.com/mcp", TokenFile: token})
	if err := os.WriteFile(global, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "upstream.json"), []byte("sensitive-token"), 0600); err != nil {
		t.Fatal(err)
	}
	lookup := func(k string) (string, bool) {
		if k == EnvConfig {
			return global, true
		}
		return "", false
	}
	if c, err := Load(dir, lookup); c != nil || err == nil || strings.Contains(err.Error(), "sensitive-token") {
		t.Fatalf("expected safe error without fallback: %v, %v", c, err)
	}
}
