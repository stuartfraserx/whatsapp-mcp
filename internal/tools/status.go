package tools

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmflurry/whatsapp-mcp/internal/session"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

const qrWait = 30 * time.Second

type emptyInput struct{}

type connectionStatusOutput struct {
	State               string  `json:"state"`
	Paired              bool    `json:"paired"`
	AccountJID          string  `json:"account_jid,omitempty"`
	SendEnabled         bool    `json:"send_enabled"`
	AllowlistActive     bool    `json:"allowlist_active"`
	RateIntervalSeconds float64 `json:"rate_interval_seconds,omitempty"`
	RateHourlyCap       int     `json:"rate_hourly_cap,omitempty"`

	ContactSync *contactSyncOut `json:"contact_sync,omitempty"`
}

type contactSyncOut struct {
	State            string         `json:"state"`
	ContactsTotal    int            `json:"contacts_total"`
	ContactsWithName int            `json:"contacts_with_name"`
	LastStarted      string         `json:"last_started,omitempty"`
	LastFinished     string         `json:"last_finished,omitempty"`
	Patches          []patchSyncOut `json:"patches"`
	Hint             string         `json:"hint,omitempty"`
}

type patchSyncOut struct {
	Name       string `json:"name"`
	Version    uint64 `json:"version"`
	LastResult string `json:"last_result,omitempty"`
	FullSync   bool   `json:"full_sync,omitempty"`
	Mutations  int    `json:"mutations,omitempty"`
	Error      string `json:"error,omitempty"`
}

const (
	hintKeysMissing = "WhatsApp has not given this device the keys to decrypt part of the account state. " +
		"The server already asked the phone for them: keep the phone online with WhatsApp open, then call resync_contacts after a few minutes. " +
		"If it stays keys_missing, unlink this device on the phone and pair again."
	hintNoNames = "No saved contact name is stored yet. Call resync_contacts to download the address book again " +
		"(it sends no message), then check contacts_with_name here after a minute."
)

func (h *handlers) contactSyncStatus(ctx context.Context) *contactSyncOut {
	st, err := h.Client.ContactSync(ctx)
	if err != nil {
		return nil
	}
	out := &contactSyncOut{
		State:            string(st.State),
		ContactsTotal:    st.ContactsTotal,
		ContactsWithName: st.ContactsNamed,
		LastStarted:      formatTime(st.LastStarted),
		LastFinished:     formatTime(st.LastFinished),
		Patches:          make([]patchSyncOut, 0, len(st.Patches)),
	}
	keysMissing := st.State == wa.ContactSyncKeysMissing
	for _, p := range st.Patches {
		keysMissing = keysMissing || p.LastResult == wa.PatchKeysMissing
		out.Patches = append(out.Patches, patchSyncOut(p))
	}
	switch {
	case keysMissing:
		out.Hint = hintKeysMissing
	case st.ContactsNamed == 0 && st.State != wa.ContactSyncRunning:
		out.Hint = hintNoNames
	}
	return out
}

type resyncOutput struct {
	Status  string `json:"status" jsonschema:"started, already_running or too_soon"`
	Message string `json:"message"`
}

func (h *handlers) resyncContacts(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, resyncOutput, error) {
	outcome, err := h.Client.ResyncContacts(ctx)
	switch {
	case errors.Is(err, wa.ErrNotPaired), errors.Is(err, wa.ErrNotConnected):
		return nil, resyncOutput{}, fmt.Errorf("cannot resync contacts: %w", err)
	case err != nil:
		return nil, resyncOutput{}, fmt.Errorf("resync contacts: %w", err)
	}
	out := resyncOutput{Status: string(outcome)}
	switch outcome {
	case wa.ResyncStarted:
		out.Message = "Address-book sync started in the background. No chat message was sent. Check contact_sync in connection_status in about a minute."
	case wa.ResyncAlreadyRunning:
		out.Message = "An address-book sync is already running; check contact_sync in connection_status."
	default:
		out.Message = "An address-book sync ran less than two minutes ago; try again later."
	}
	return nil, out, nil
}

func (h *handlers) connectionStatus(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, connectionStatusOutput, error) {
	st := h.Session.Status()
	out := connectionStatusOutput{
		State:           string(st.State),
		Paired:          st.Paired,
		AccountJID:      st.AccountJID,
		SendEnabled:     h.SendEnabled,
		AllowlistActive: h.AllowlistActive || (h.Policy != nil && !h.Policy.Allowlist.Empty()),
	}
	if h.SendEnabled {
		out.RateIntervalSeconds = h.RateInterval.Seconds()
		out.RateHourlyCap = h.RateHourly
	}
	if st.Paired {
		out.ContactSync = h.contactSyncStatus(ctx)
	}
	return nil, out, nil
}

type pairingQROutput struct {
	Status             string `json:"status" jsonschema:"already_paired or scan_required"`
	PNGPath            string `json:"png_path,omitempty"`
	ExpiresAt          string `json:"expires_at,omitempty"`
	RenderedInTerminal bool   `json:"rendered_in_terminal"`
	Instructions       string `json:"instructions"`
}

func (h *handlers) showPairingQR(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, pairingQROutput, error) {
	alreadyPaired := pairingQROutput{
		Status:       "already_paired",
		Instructions: "This WhatsApp account is already paired; no QR code is needed.",
	}
	if h.Session.Status().Paired {
		return nil, alreadyPaired, nil
	}

	path, expires, err := h.Session.EnsureQR(ctx, qrWait)
	if errors.Is(err, session.ErrAlreadyPaired) {
		return nil, alreadyPaired, nil
	}
	if err != nil {
		return nil, pairingQROutput{}, fmt.Errorf("could not start pairing: %w", err)
	}
	return nil, pairingQROutput{
		Status:             "scan_required",
		PNGPath:            path,
		ExpiresAt:          formatTime(expires),
		RenderedInTerminal: h.Session.QRRendered(),
		Instructions: "Open the PNG at png_path on the machine running this server, then on your phone open " +
			"WhatsApp > Settings > Linked devices > Link a device and scan it. The code expires at expires_at; " +
			"call this tool again for a fresh one, and connection_status to confirm pairing.",
	}, nil
}
