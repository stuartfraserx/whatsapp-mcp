package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

// StatusViewsQuery selects own statuses within a window, or one status by ID.
type StatusViewsQuery struct {
	MessageID string
	Since     time.Time
}

// StatusViews includes the stored caption and all received view receipts.
type StatusViews struct {
	MessageID string
	PostedAt  time.Time
	Text      string
	Viewers   []wa.StatusView
}

// SaveStatusViews stores each receipt once, keeping the first timestamp.
func (s *Store) SaveStatusViews(ctx context.Context, views []wa.StatusView) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, v := range views {
		if v.MessageID == "" || v.ViewerJID == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO status_views(message_id,viewer_jid,receipt_type,ts) VALUES(?,?,?,?)`, v.MessageID, v.ViewerJID, v.ReceiptType, timeToInt(v.Timestamp)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListStatusViews includes stored own statuses with no views and orphan receipts.
func (s *Store) ListStatusViews(ctx context.Context, q StatusViewsQuery) ([]StatusViews, error) {
	rows, err := s.db.QueryContext(ctx, `WITH statuses AS (
 SELECT id AS message_id, ts AS posted_at, text FROM messages WHERE chat_jid='status@broadcast' AND from_me=1
 UNION ALL
 SELECT message_id, 0, '' FROM status_views v WHERE NOT EXISTS (SELECT 1 FROM messages m WHERE m.chat_jid='status@broadcast' AND m.id=v.message_id) GROUP BY message_id
 ), selected AS (
 SELECT * FROM statuses s WHERE (? != '' AND message_id=?) OR (? = '' AND (posted_at >= ? OR (posted_at=0 AND EXISTS (SELECT 1 FROM status_views v WHERE v.message_id=s.message_id AND v.ts>=?))))
 )
 SELECT s.message_id,s.posted_at,s.text,v.viewer_jid,v.receipt_type,v.ts FROM selected s LEFT JOIN status_views v ON v.message_id=s.message_id
 ORDER BY s.posted_at DESC,s.message_id,v.ts,v.viewer_jid,v.receipt_type`, q.MessageID, q.MessageID, q.MessageID, timeToInt(q.Since), timeToInt(q.Since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StatusViews{}
	for rows.Next() {
		var id, text string
		var posted int64
		var jid, kind sql.NullString
		var ts sql.NullInt64
		if err := rows.Scan(&id, &posted, &text, &jid, &kind, &ts); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].MessageID != id {
			out = append(out, StatusViews{MessageID: id, PostedAt: intToTime(posted), Text: text, Viewers: []wa.StatusView{}})
		}
		if jid.Valid {
			i := len(out) - 1
			out[i].Viewers = append(out[i].Viewers, wa.StatusView{MessageID: id, ViewerJID: jid.String, ReceiptType: kind.String, Timestamp: intToTime(ts.Int64)})
		}
	}
	return out, rows.Err()
}
