package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrateV2(t *testing.T) {
	for _, version := range []int{0, 1} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "messages.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx := context.Background()
			if version == 1 {
				if _, err = db.Exec(schemaV1); err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec(`INSERT INTO messages(chat_jid,id,ts,from_me,text) VALUES('chat','id',123,1,'original'); PRAGMA user_version=1`); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				if err = migrate(ctx, db); err != nil {
					t.Fatal(err)
				}
			}
			var got int
			if err = db.QueryRow("PRAGMA user_version").Scan(&got); err != nil || got != 2 {
				t.Fatalf("version=%d err=%v", got, err)
			}
			if version == 0 {
				if _, err = db.Exec(`INSERT INTO messages(chat_jid,id,ts,from_me,text) VALUES('chat','id',123,1,'original')`); err != nil {
					t.Fatal(err)
				}
			}
			s := &Store{db: db}
			m, err := s.GetMessage(ctx, "chat", "id")
			if err != nil || m.Text != "original" || m.Edited || m.Revoked || !m.EditedAt.IsZero() || !m.RevokedAt.IsZero() {
				t.Fatalf("message=%+v err=%v", m, err)
			}
			if err = s.ApplyEdit(ctx, "chat", "id", "", "edited"); err != nil {
				t.Fatal(err)
			}
			m, err = s.GetMessage(ctx, "chat", "id")
			if err != nil || !m.Edited || m.EditedAt.IsZero() || m.Text != "edited" || !m.Timestamp.Equal(time.Unix(0, 123)) {
				t.Fatalf("message=%+v err=%v", m, err)
			}
		})
	}
}

func TestMigrationFailureRollsBack(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(schemaV1); err != nil {
		t.Fatal(err)
	}
	// An inconsistent schema fails after the first ALTER; none of the partial
	// migration should persist and its version must remain unchanged.
	if _, err = db.Exec(`ALTER TABLE messages ADD COLUMN edited_at INTEGER NOT NULL DEFAULT 0; PRAGMA user_version=1`); err != nil {
		t.Fatal(err)
	}
	if err = migrate(context.Background(), db); err == nil {
		t.Fatal("expected migration failure")
	}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if _, err = db.Exec("SELECT edited FROM messages"); err == nil {
		t.Fatal("partial migration was committed")
	}
}
