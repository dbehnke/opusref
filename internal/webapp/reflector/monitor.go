// Package reflector adapts the loopback reflector monitor without proxying it.
package reflector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

type ClientInfo struct {
	NodeCallsign string    `json:"node_callsign"`
	ConnectedAt  time.Time `json:"connected_at"`
	LastActivity time.Time `json:"last_activity"`
}
type Stream struct {
	Active         bool      `json:"active"`
	SourceCallsign string    `json:"source_callsign"`
	StartedAt      time.Time `json:"started_at"`
	Remaining      float64   `json:"remaining_transmit_seconds"`
}
type Snapshot struct {
	ReflectorID string       `json:"reflector_id"`
	DisplayName string       `json:"display_name"`
	Ready       bool         `json:"ready"`
	ClientCount int          `json:"client_count"`
	Clients     []ClientInfo `json:"-"`
	Stream      Stream       `json:"-"`
	Updated     time.Time    `json:"-"`
}
type Client struct {
	base     string
	http     *http.Client
	snapshot atomic.Pointer[Snapshot]
}

func New(base string) *Client {
	return &Client{strings.TrimRight(base, "/"), &http.Client{Timeout: 2 * time.Second}, atomic.Pointer[Snapshot]{}}
}
func (c *Client) Poll(ctx context.Context) error {
	var status Snapshot
	if err := c.get(ctx, "/api/v1/status", &status); err != nil {
		return err
	}
	var clients struct {
		Clients []ClientInfo `json:"clients"`
	}
	if err := c.get(ctx, "/api/v1/clients", &clients); err != nil {
		return err
	}
	var stream struct {
		Stream Stream `json:"stream"`
	}
	if err := c.get(ctx, "/api/v1/stream", &stream); err != nil {
		return err
	}
	status.Clients = clients.Clients
	status.Stream = stream.Stream
	status.Updated = time.Now()
	c.snapshot.Store(&status)
	return nil
}
func (c *Client) get(ctx context.Context, path string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("reflector monitor is unavailable")
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(target)
}
func (c *Client) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		_ = c.Poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (c *Client) Snapshot() (Snapshot, bool) {
	value := c.snapshot.Load()
	if value == nil {
		return Snapshot{}, false
	}
	copy := *value
	copy.Clients = append([]ClientInfo(nil), value.Clients...)
	return copy, true
}
func (c *Client) Fresh(maxAge time.Duration) bool {
	snapshot, ok := c.Snapshot()
	return ok && time.Since(snapshot.Updated) <= maxAge
}
