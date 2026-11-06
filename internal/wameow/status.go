package wameow

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

// ReadStatus inspects pairing without creating, migrating or updating files.
func ReadStatus(ctx context.Context, path string) (wa.Status, error) {
	st := wa.Status{State: wa.StateDisconnected}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return st, nil
	} else if err != nil {
		return st, err
	}
	u := &url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=query_only(1)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return st, err
	}
	defer db.Close()
	var count int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='whatsmeow_device'`).Scan(&count); err != nil || count == 0 {
		return st, err
	}
	var jid string
	err = db.QueryRowContext(ctx, `SELECT jid FROM whatsmeow_device LIMIT 1`).Scan(&jid)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.Paired = true
	st.AccountJID = wa.User(jid) + "@" + wa.Server(jid)
	return st, nil
}
