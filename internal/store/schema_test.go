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
			if err = db.QueryRow("PRAGMA user_version").Scan(&got); err != nil || got != schemaVersion {
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

func TestMigratePopulatedV2(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err = migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DROP TABLE status_views;
 INSERT INTO chats(jid,name,is_group) VALUES('chat','Saved',0),('status@broadcast','Status',0);
 INSERT INTO messages(chat_jid,id,ts,from_me,text,edited,edited_at,revoked,revoked_at) VALUES('chat','one',123,0,'hello',1,12,1,13),('status@broadcast','two',456,1,'caption',0,0,0,0);
 PRAGMA user_version=2`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	var version, chats, messages int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM chats WHERE (jid='chat' AND name='Saved') OR (jid='status@broadcast' AND name='Status')`).Scan(&chats); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM messages WHERE (id='one' AND chat_jid='chat' AND ts=123 AND text='hello' AND edited=1 AND edited_at=12 AND revoked=1 AND revoked_at=13) OR (id='two' AND chat_jid='status@broadcast' AND ts=456 AND from_me=1 AND text='caption')`).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if version != 3 || chats != 2 || messages != 2 {
		t.Fatalf("version=%d chats=%d messages=%d", version, chats, messages)
	}
	if _, err = db.Exec(`SELECT message_id,viewer_jid,receipt_type,ts FROM status_views`); err != nil {
		t.Fatal(err)
	}
}
