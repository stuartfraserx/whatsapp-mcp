package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoveryPrivateAndValidated(t *testing.T) {
	dir := t.TempDir()
	want := Info{HTTPAddr: "127.0.0.1:8765", TokenFile: filepath.Join(dir, "token"), StartCommand: []string{"/bin/false"}}
	if err := Write(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.HTTPAddr != want.HTTPAddr || got.TokenFile != want.TokenFile || got.PID != os.Getpid() || got.StartedAt == "" || len(got.StartCommand) != 1 {
		t.Fatalf("metadata %+v", got)
	}
	os.Chmod(Path(dir), 0644)
	if _, err := Read(dir); err == nil {
		t.Fatal("accepted public metadata")
	}
	os.Chmod(Path(dir), 0600)
	os.Remove(Path(dir))
	if err := os.Symlink(want.TokenFile, Path(dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("accepted symlink")
	}
	os.Remove(Path(dir))
	for _, b := range []string{`{`, `{"http_addr":"evil.test:8765"}`, `{"http_addr":"127.0.0.1:8765","token_file":"relative"}`} {
		os.WriteFile(Path(dir), []byte(b), 0600)
		if _, err := Read(dir); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
}
