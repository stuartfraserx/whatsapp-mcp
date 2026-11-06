package wameow

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/redact"
)

type reconnecter struct {
	mu               sync.Mutex
	enabled, running bool
	loggedOut        bool
	count            uint64
	runID            uint64
	reason           string
	delay            time.Duration
	connectedAt      time.Time
	now              func() time.Time
	sleep            func(context.Context, time.Duration) error
	connect          func(context.Context) error
	log              *slog.Logger
}

func newReconnecter(log *slog.Logger, connect func(context.Context) error) *reconnecter {
	return &reconnecter{log: log, connect: connect, now: time.Now, sleep: sleepCtx}
}
func (r *reconnecter) connected() {
	r.mu.Lock()
	r.loggedOut = false
	r.connectedAt = r.now()
	r.mu.Unlock()
}
func (r *reconnecter) disconnected(reason string) {
	r.mu.Lock()
	if r.reason != "stream_replaced" || !r.connectedAt.IsZero() || reason != "disconnected" {
		r.reason = reason
	}
	r.connectedAt = time.Time{}
	r.mu.Unlock()
}
func (r *reconnecter) replaced(ctx context.Context) {
	r.mu.Lock()
	r.count++
	r.reason = "stream_replaced"
	if !r.connectedAt.IsZero() && r.now().Sub(r.connectedAt) >= 10*time.Minute {
		r.delay = 0
	}
	r.connectedAt = time.Time{}
	if !r.enabled || r.loggedOut || r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.runID++
	id := r.runID
	r.mu.Unlock()
	go r.run(ctx, id)
}
func (r *reconnecter) run(ctx context.Context, id uint64) {
	defer func() {
		if recover() != nil {
			r.log.Error("stream reconnect panic recovered")
		}
		r.mu.Lock()
		if r.runID == id {
			r.running = false
		}
		r.mu.Unlock()
	}()
	for {
		r.mu.Lock()
		if r.delay == 0 {
			r.delay = 5 * time.Second
		} else {
			r.delay = min(2*r.delay, 5*time.Minute)
		}
		delay := r.delay
		enabled := r.enabled && !r.loggedOut
		r.mu.Unlock()
		if !enabled {
			return
		}
		if r.sleep(ctx, delay) != nil || ctx.Err() != nil {
			return
		}
		r.mu.Lock()
		generation, enabled := r.count, r.enabled && !r.loggedOut
		r.mu.Unlock()
		if !enabled {
			return
		}
		if err := r.connect(ctx); err == nil {
			r.mu.Lock()
			if r.count == generation {
				r.running = false
				r.mu.Unlock()
				return
			}
			r.mu.Unlock()
		} else if ctx.Err() == nil {
			r.log.Warn("stream replacement reconnect failed", "error", redact.Scrub(err.Error()), "retry_delay", min(delay*2, 5*time.Minute))
		}
	}
}

// SetInstanceOwner permits stream-replacement recovery only for the lock owner.
func (c *Client) SetInstanceOwner(owned bool) {
	c.reconnect.mu.Lock()
	c.reconnect.enabled = owned
	c.reconnect.mu.Unlock()
}
