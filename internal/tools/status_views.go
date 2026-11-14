package tools

import (
	"cmp"
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmflurry/whatsapp-mcp/internal/store"
)

type statusViewsInput struct {
	MessageID string `json:"message_id,omitempty" jsonschema:"One status message ID, regardless of age"`
	Since     string `json:"since,omitempty" jsonschema:"RFC3339 timestamp; defaults to the last 48 hours"`
}
type statusViewerOut struct {
	JID         string `json:"jid"`
	Phone       string `json:"phone,omitempty"`
	Name        string `json:"name,omitempty"`
	PushName    string `json:"push_name,omitempty"`
	ViewedAt    string `json:"viewed_at"`
	ReceiptType string `json:"receipt_type"`
}
type statusViewsEntry struct {
	MessageID         string            `json:"message_id"`
	PostedAt          string            `json:"posted_at,omitempty"`
	CaptionSnippet    string            `json:"caption_snippet"`
	UniqueViewerCount int               `json:"unique_viewer_count"`
	Viewers           []statusViewerOut `json:"viewers"`
}
type statusViewsOutput struct {
	Statuses []statusViewsEntry `json:"statuses"`
}

func (h *handlers) statusViews(ctx context.Context, _ *mcp.CallToolRequest, in statusViewsInput) (*mcp.CallToolResult, statusViewsOutput, error) {
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	since := now().UTC().Add(-48 * time.Hour)
	if in.Since != "" {
		var err error
		since, err = time.Parse(time.RFC3339, in.Since)
		if err != nil {
			return nil, statusViewsOutput{}, fmt.Errorf("invalid since: %w", err)
		}
	}
	statuses, err := h.Store.ListStatusViews(ctx, store.StatusViewsQuery{MessageID: in.MessageID, Since: since})
	if err != nil {
		return nil, statusViewsOutput{}, fmt.Errorf("list status views: %w", err)
	}
	dir := h.contactDirectory(ctx)
	chats, _, err := h.scanChats(ctx, "", chatScanPages)
	if err == nil {
		for _, c := range chats {
			dir.addChat(c)
		}
	}
	out := statusViewsOutput{Statuses: []statusViewsEntry{}}
	for _, s := range statuses {
		caption := []rune(s.Text)
		snippet := s.Text
		if len(caption) > 140 {
			snippet = string(caption[:140]) + "…"
		}
		entry := statusViewsEntry{MessageID: s.MessageID, PostedAt: formatTime(s.PostedAt), CaptionSnippet: snippet, Viewers: []statusViewerOut{}}
		// Receipts arrive sorted by time. A read wins a tie with played.
		seen := map[string]int{}
		for receiptIndex, v := range s.Viewers {
			if index, ok := seen[v.ViewerJID]; ok {
				first := s.Viewers[index]
				if v.Timestamp.Equal(first.Timestamp) && v.ReceiptType == "read" {
					for i := range entry.Viewers {
						if entry.Viewers[i].JID == v.ViewerJID {
							entry.Viewers[i].ReceiptType = "read"
							break
						}
					}
				}
				continue
			}
			seen[v.ViewerJID] = receiptIndex

			viewer := statusViewerOut{JID: v.ViewerJID, Phone: phoneOf(v.ViewerJID), ViewedAt: formatTime(v.Timestamp), ReceiptType: v.ReceiptType}
			if p := dir.lookup(v.ViewerJID); p != nil {
				viewer.Name = cmp.Or(p.displayName(), p.push)
				viewer.PushName = p.push
			}
			entry.Viewers = append(entry.Viewers, viewer)
		}
		entry.UniqueViewerCount = len(entry.Viewers)
		out.Statuses = append(out.Statuses, entry)
	}
	return nil, out, nil
}
