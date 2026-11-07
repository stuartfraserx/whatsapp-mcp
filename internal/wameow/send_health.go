package wameow

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func (c *Client) clockNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}
func (c *Client) SetSendTimeout(d time.Duration) { c.mu.Lock(); c.SendTimeout = d; c.mu.Unlock() }
func (c *Client) sendTimeout() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.SendTimeout > 0 {
		return c.SendTimeout
	}
	return 25 * time.Second
}
func (c *Client) HealthyForSend() bool {
	c.mu.Lock()
	stale := c.healing || (c.keepAliveCount > 0 && c.clockNow().Sub(c.lastKeepAlive) < 2*time.Minute)
	c.mu.Unlock()
	return !stale && c.connectionReady()
}
func (c *Client) keepAliveTimeout() {
	c.mu.Lock()
	c.keepAliveCount++
	c.lastKeepAlive = c.clockNow()
	heal := c.keepAliveCount >= 2
	c.mu.Unlock()
	if heal {
		c.selfHeal("keepalive timeouts")
	}
}
func (c *Client) selfHeal(reason string) {
	c.mu.Lock()
	now := c.clockNow()
	c.keepAliveCount = max(c.keepAliveCount, 1)
	c.lastKeepAlive = now
	if c.healing || (!c.lastHeal.IsZero() && now.Sub(c.lastHeal) < 30*time.Second) {
		c.mu.Unlock()
		return
	}
	// Never enter Connect's pairing path during recovery.
	if c.healHook == nil && (c.cli == nil || c.cli.Store.ID == nil || c.loggedOut) {
		c.mu.Unlock()
		return
	}
	c.healing = true
	c.lastHeal = now
	c.mu.Unlock()
	go func() {
		defer func() { c.mu.Lock(); c.healing = false; c.mu.Unlock() }()
		if c.log != nil {
			c.log.Warn("whatsapp self-heal reconnect", "reason", reason)
		}
		ctx := c.lifeCtx
		if ctx == nil {
			ctx = context.Background()
		}
		// Keep the connection context alive after success: whatsmeow also uses
		// it for the socket read pump and keepalive loop.
		c.opMu.Lock()
		defer c.opMu.Unlock()
		started := time.Now()
		var err error
		for attempt := 1; attempt <= 2 && ctx.Err() == nil; attempt++ {
			c.mu.Lock()
			forbidden := c.loggedOut
			c.mu.Unlock()
			if c.healHook == nil && (forbidden || c.cli.Store.ID == nil) {
				err = errors.New("session no longer paired")
				break
			}
			c.forceCloseRaw()
			if c.disconnectHook != nil {
				c.disconnectHook()
			} else if c.cli != nil {
				c.cli.Disconnect()
			}
			attemptCtx, cancel := context.WithCancel(ctx)
			timerDone := make(chan struct{})
			timer := time.AfterFunc(15*time.Second, func() {
				cancel()
				// Also interrupt handshake writes that may not observe ctx.
				c.forceCloseRaw()
				close(timerDone)
			})
			if c.healHook != nil {
				err = c.healHook(attemptCtx)
			} else {
				err = c.cli.ConnectContext(attemptCtx)
			}
			if err == nil && (c.connectedHook != nil || c.healHook == nil) {
				ticker := time.NewTicker(50 * time.Millisecond)
				for !c.connectionReady() && attemptCtx.Err() == nil {
					select {
					case <-ticker.C:
					case <-attemptCtx.Done():
					}
				}
				ticker.Stop()
				err = attemptCtx.Err()
			}
			stopped := timer.Stop()
			if !stopped {
				<-timerDone
			}
			if err == nil && stopped {
				if c.log != nil {
					c.log.Info("whatsapp self-heal ok", "attempt", attempt, "duration", time.Since(started))
				}
				return
			}
			cancel()
			if err == nil {
				err = context.DeadlineExceeded
			}
			if c.log != nil {
				c.log.Warn("whatsapp self-heal attempt failed", "attempt", attempt, "duration", time.Since(started))
			}
		}
		c.forceCloseRaw()
		if c.log != nil {
			c.log.Error("whatsapp self-heal failed", "duration", time.Since(started), "error", err)
		}

	}()
}
func (c *Client) send(ctx context.Context, jid types.JID, msg *waE2E.Message) (wa.SentMessage, error) {
	if ctx.Err() != nil {
		return wa.SentMessage{}, wa.ErrNotSentStale
	}
	c.mu.Lock()
	if c.sendGate == nil {
		c.sendGate = make(chan struct{}, 1)
	}
	gate := c.sendGate
	c.mu.Unlock()
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return wa.SentMessage{}, wa.ErrNotSentStale
	}
	if ctx.Err() != nil || (c.sendHook == nil && !c.HealthyForSend()) {
		<-gate
		return wa.SentMessage{}, wa.ErrNotSentStale
	}
	var id string
	if c.idHook != nil {
		id = c.idHook()
	} else {
		id = c.cli.GenerateMessageID()
	}
	type result struct {
		resp whatsmeow.SendResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-gate }()
		send := c.sendHook
		if send == nil {
			send = func(ctx context.Context, jid types.JID, msg *waE2E.Message, extra whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
				return c.cli.SendMessage(ctx, jid, msg, extra)
			}
		}
		resp, err := send(ctx, jid, msg, whatsmeow.SendRequestExtra{ID: id, Timeout: c.sendTimeout()})
		done <- result{resp, err}
	}()
	var resp whatsmeow.SendResponse
	var err error
	select {
	case r := <-done:
		resp, err = r.resp, r.err
	case <-ctx.Done():
		select {
		case r := <-done:
			resp, err = r.resp, r.err
		default:
			err = ctx.Err()
		}
	}
	if err != nil {
		failure := sendFailure(err)
		if ctx.Err() != nil || staleTimeout(err) {
			c.selfHeal("send timeout")
		}
		if errors.Is(failure, wa.ErrNotSent) {
			return wa.SentMessage{}, wa.ErrNotSentStale
		}
		return wa.SentMessage{ID: id, ChatJID: jid.String()}, &wa.MaybeSentError{MessageID: id}
	}
	c.mu.Lock()
	c.keepAliveCount = 0
	c.mu.Unlock()
	return sentMessage(resp, jid), nil
}

// upload never advances to SendMessage after the caller's deadline, even if an
// upload transport is slow to observe cancellation.
func (c *Client) upload(ctx context.Context, data []byte, kind wa.MediaType) (uploaded, error) {
	if err := ctx.Err(); err != nil {
		return uploaded{}, err
	}
	type result struct {
		up  uploaded
		err error
	}
	done := make(chan result, 1)
	go func() {
		if c.uploadHook != nil {
			up, err := c.uploadHook(ctx, data, kind)
			done <- result{up, err}
			return
		}
		up, err := c.cli.Upload(ctx, data, whatsmeowMediaType(kind))
		done <- result{uploaded{URL: up.URL, DirectPath: up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: up.FileLength}, err}
	}()
	select {
	case r := <-done:
		return r.up, r.err
	case <-ctx.Done():
		return uploaded{}, ctx.Err()
	}
}

func staleTimeout(err error) bool {
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || (errors.As(err, &network) && network.Timeout()) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "timed out") || strings.Contains(text, "timeout") || strings.Contains(text, "deadline exceeded")
}

func (c *Client) connectionReady() bool {
	if c.connectedHook != nil {
		return c.connectedHook()
	}
	return c.cli != nil && c.cli.IsConnected() && c.cli.IsLoggedIn()
}
