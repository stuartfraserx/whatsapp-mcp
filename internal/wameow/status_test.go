package wameow

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestReadStatusIsReadOnlyWithoutMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whatsapp.db")
	st, err := ReadStatus(context.Background(), path)
	if err != nil || st.Paired {
		t.Fatalf("missing: %+v %v", st, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("read created database")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE whatsmeow_device(jid TEXT); INSERT INTO whatsmeow_device VALUES('123456789:2@s.whatsapp.net'); PRAGMA user_version=999`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err = ReadStatus(context.Background(), path)
	if err != nil || !st.Paired || st.AccountJID != "123456789@s.whatsapp.net" {
		t.Fatalf("paired: %+v %v", st, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("status modified database")
	}
}
