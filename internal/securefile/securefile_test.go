package securefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateRegularFilesOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(path, []byte("private-data"), 0600); err != nil {
		t.Fatal(err)
	}
	if b, err := Read(path, true); err != nil || string(b) != "private-data" {
		t.Fatalf("read: %q %v", b, err)
	}
	os.Chmod(path, 0400)
	if _, err := Read(path, false); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path, true); err == nil {
		t.Fatal("exact accepted 0400")
	}
	os.Chmod(path, 0640)
	if _, err := Read(path, false); err == nil {
		t.Fatal("accepted group permission")
	}
	link := path + "-link"
	os.Symlink(path, link)
	if _, err := Read(link, false); err == nil {
		t.Fatal("accepted symlink")
	}
	if _, err := Read(filepath.Dir(path), false); err == nil {
		t.Fatal("accepted directory")
	}
}
