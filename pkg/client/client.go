// Package client provides the raw-frame OpusRef client contract.
package client

import (
	"context"
	"errors"
	"github.com/dbehnke/opusref/pkg/wire"
	"sync"
	"time"
)

var (
	ErrBackpressure            = errors.New("client media queue is full")
	ErrApplicationBackpressure = errors.New("client application event queue is full")
	ErrControlBackpressure     = errors.New("client control queue is full")
	ErrNotConnected            = errors.New("client is not connected")
	ErrStreamInactive          = errors.New("client stream is not active")
	ErrConnectInProgress       = errors.New("client connection is in progress")
)

// ProtocolError is a correlated ERROR response from the reflector.
type ProtocolError struct {
	Code    uint16
	Message string
}

func (e *ProtocolError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "reflector returned protocol error"
}

type EventKind uint8

const (
	EventStreamStart EventKind = iota + 1
	EventAudio
	EventData
	EventStreamEnd
	EventBusy
	EventStatus
	EventProtocolError
	EventStreamGranted
)

type Event struct {
	Kind                          EventKind
	SessionID                     uint64
	StreamID, Sequence, Timestamp uint32
	DataType                      uint16
	NodeCallsign, SourceCallsign  string
	Payload                       []byte
	ProtocolErrorCode             uint16
	Message                       string
	EndReason                     wire.EndReason
	Synthetic                     bool
}
type Options struct {
	ServerAddress, NodeCallsign, SharedKey                                                     string
	InboundQueuePackets, ApplicationQueueEvents, MediaSendQueueFrames, ControlSendQueuePackets int
	ConnectTimeout, OperationTimeout                                                           time.Duration
}

func (o Options) defaults() Options {
	if o.InboundQueuePackets == 0 {
		o.InboundQueuePackets = 256
	}
	if o.ApplicationQueueEvents == 0 {
		o.ApplicationQueueEvents = 256
	}
	if o.MediaSendQueueFrames == 0 {
		o.MediaSendQueueFrames = 256
	}
	if o.ControlSendQueuePackets == 0 {
		o.ControlSendQueuePackets = 32
	}
	if o.ConnectTimeout == 0 {
		o.ConnectTimeout = 10 * time.Second
	}
	if o.OperationTimeout == 0 {
		o.OperationTimeout = 5 * time.Second
	}
	return o
}

func (o Options) validate() error {
	if o.InboundQueuePackets <= 0 || o.ApplicationQueueEvents <= 0 || o.MediaSendQueueFrames <= 0 || o.ControlSendQueuePackets <= 0 {
		return errors.New("all client queue capacities must be positive")
	}
	if o.ConnectTimeout <= 0 || o.OperationTimeout <= 0 {
		return errors.New("all client timeouts must be positive")
	}
	return nil
}

type Outbound struct {
	Kind                          EventKind
	StreamID, Sequence, Timestamp uint32
	DataType                      uint16
	SourceCallsign                string
	Payload                       []byte
}
type Sender interface {
	Send(context.Context, Outbound) error
}
type floorRequester interface {
	RequestFloor(context.Context, Outbound) error
}
type streamEnder interface {
	EndFloor(context.Context, Outbound) error
}
type disconnecter interface {
	Disconnect(context.Context) error
}
type Client interface {
	Connect(context.Context) error
	RequestStream(context.Context, string) error
	SendAudio(context.Context, uint32, []byte) error
	SendData(context.Context, uint32, uint16, []byte) error
	EndStream(context.Context) error
	Events() <-chan Event
	Done() <-chan struct{}
	Err() error
	Close() error
}

// QueueClient applies queue and ownership rules around an injected sender.
type QueueClient struct {
	mu                                    sync.Mutex
	options                               Options
	sender                                Sender
	events                                chan Event
	media                                 chan Outbound
	mediaProgress                         chan struct{}
	control                               chan Outbound
	done                                  chan struct{}
	connected, connecting, stream, ending bool
	requesting                            bool
	streamID, sequence                    uint32
	lastTimestamp                         uint32
	mediaAccepted, mediaSent              uint64
	terminal, closeResult                 error
	closeCalled                           bool
	closeDone                             chan struct{}
	once                                  sync.Once
}

func New(options Options, sender Sender) (*QueueClient, error) {
	options = options.defaults()
	if err := options.validate(); err != nil {
		return nil, err
	}
	if options.ServerAddress == "" || options.NodeCallsign == "" {
		return nil, errors.New("server address and node callsign are required")
	}
	if sender == nil {
		return nil, errors.New("sender is required")
	}
	return &QueueClient{options: options, sender: sender, events: make(chan Event, options.ApplicationQueueEvents), media: make(chan Outbound, options.MediaSendQueueFrames), mediaProgress: make(chan struct{}, 1), control: make(chan Outbound, options.ControlSendQueuePackets), done: make(chan struct{}), closeDone: make(chan struct{})}, nil
}
func (c *QueueClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	if c.connected {
		c.mu.Unlock()
		return nil
	}
	if c.connecting {
		c.mu.Unlock()
		return ErrConnectInProgress
	}
	c.connecting = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.connecting = false; c.mu.Unlock() }()
	connectCtx, cancel := context.WithTimeout(ctx, c.options.ConnectTimeout)
	defer cancel()
	if err := c.sender.Send(connectCtx, Outbound{Kind: EventStatus}); err != nil {
		return err
	}
	c.mu.Lock()
	c.connected = true
	c.mu.Unlock()
	go c.run()
	return c.publish(Event{Kind: EventStatus, Message: "connected"}, true)
}
func (c *QueueClient) RequestStream(ctx context.Context, source string) error {
	return c.requestStream(ctx, source)
}
func (c *QueueClient) requestStream(ctx context.Context, source string) error {
	c.mu.Lock()
	if !c.connected {
		c.mu.Unlock()
		return ErrNotConnected
	}
	if source == "" {
		c.mu.Unlock()
		return errors.New("source callsign is required")
	}
	if c.stream || c.requesting || c.ending {
		c.mu.Unlock()
		return errors.New("stream request is already active")
	}
	nextStream := c.streamID + 1
	if nextStream == 0 {
		nextStream++
	}
	c.requesting = true
	out := Outbound{Kind: EventStreamStart, StreamID: nextStream, SourceCallsign: source}
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.requesting = false; c.mu.Unlock() }()
	if err := c.acquireControl(out); err != nil {
		return err
	}
	defer c.releaseControl()
	if requester, ok := c.sender.(floorRequester); ok {
		if err := requester.RequestFloor(ctx, out); err != nil {
			return err
		}
	} else if err := c.sender.Send(ctx, out); err != nil {
		return err
	}
	c.mu.Lock()
	c.streamID = nextStream
	c.sequence = 0
	c.lastTimestamp = 0
	c.stream = true
	c.mu.Unlock()
	return nil
}
func (c *QueueClient) SendAudio(_ context.Context, timestamp uint32, payload []byte) error {
	if len(payload) < 1 || len(payload) > 1168 {
		return errors.New("audio payload length is invalid")
	}
	return c.enqueueMedia(Outbound{Kind: EventAudio, Timestamp: timestamp, Payload: append([]byte(nil), payload...)})
}
func (c *QueueClient) SendData(_ context.Context, timestamp uint32, typ uint16, payload []byte) error {
	if typ == 0 || len(payload) < 1 || len(payload) > 1160 {
		return errors.New("data record is invalid")
	}
	return c.enqueueMedia(Outbound{Kind: EventData, Timestamp: timestamp, DataType: typ, Payload: append([]byte(nil), payload...)})
}
func (c *QueueClient) enqueueMedia(out Outbound) error {
	c.mu.Lock()
	if !c.stream {
		c.mu.Unlock()
		return ErrStreamInactive
	}
	out.StreamID = c.streamID
	out.Sequence = c.sequence
	select {
	case c.media <- out:
		c.sequence++
		c.lastTimestamp = out.Timestamp
		c.mediaAccepted++
		c.mu.Unlock()
		return nil
	default:
		c.mu.Unlock()
		return ErrBackpressure
	}
}
func (c *QueueClient) acquireControl(out Outbound) error {
	select {
	case c.control <- out:
		return nil
	default:
		c.fail(ErrControlBackpressure)
		return ErrControlBackpressure
	}
}
func (c *QueueClient) releaseControl() { <-c.control }
func (c *QueueClient) EndStream(ctx context.Context) error {
	c.mu.Lock()
	if !c.stream {
		c.mu.Unlock()
		return ErrStreamInactive
	}
	out := Outbound{Kind: EventStreamEnd, StreamID: c.streamID, Sequence: c.sequence, Timestamp: c.lastTimestamp}
	target := c.mediaAccepted
	c.stream = false
	c.ending = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.ending = false; c.mu.Unlock() }()
	if err := c.waitMedia(ctx, target); err != nil {
		return err
	}
	if err := c.acquireControl(out); err != nil {
		return err
	}
	defer c.releaseControl()
	if ender, ok := c.sender.(streamEnder); ok {
		return ender.EndFloor(ctx, out)
	}
	return c.sender.Send(ctx, out)
}
func (c *QueueClient) run() {
	for {
		select {
		case out := <-c.media:
			if err := c.sender.Send(context.Background(), out); err != nil {
				c.fail(err)
				return
			}
			c.mu.Lock()
			c.mediaSent++
			c.mu.Unlock()
			select {
			case c.mediaProgress <- struct{}{}:
			default:
			}
		case <-c.done:
			return
		}
	}
}
func (c *QueueClient) waitMedia(ctx context.Context, target uint64) error {
	for {
		c.mu.Lock()
		sent, terminal := c.mediaSent, c.terminal
		c.mu.Unlock()
		if sent >= target {
			return nil
		}
		if terminal != nil {
			return terminal
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return c.Err()
		case <-c.mediaProgress:
		}
	}
}
func (c *QueueClient) Publish(event Event) error {
	event.Payload = append([]byte(nil), event.Payload...)
	required := event.Kind != EventAudio && event.Kind != EventData
	return c.publish(event, required)
}
func (c *QueueClient) revoke(stream uint32) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.stream || c.streamID != stream {
		return false
	}
	c.stream = false
	return true
}
func (c *QueueClient) publish(event Event, required bool) error {
	select {
	case c.events <- event:
		return nil
	default:
		if required {
			c.fail(ErrApplicationBackpressure)
			return ErrApplicationBackpressure
		}
		return nil
	}
}
func (c *QueueClient) Events() <-chan Event  { return c.events }
func (c *QueueClient) Done() <-chan struct{} { return c.done }
func (c *QueueClient) Err() error            { c.mu.Lock(); defer c.mu.Unlock(); return c.terminal }
func (c *QueueClient) fail(err error) {
	c.mu.Lock()
	if c.terminal == nil {
		c.terminal = err
	}
	c.connected = false
	c.stream = false
	c.mu.Unlock()
	c.once.Do(func() { close(c.done) })
	if closer, ok := c.sender.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}
func (c *QueueClient) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), c.options.OperationTimeout)
	defer cancel()
	return c.closeContext(ctx)
}

func (c *QueueClient) closeContext(ctx context.Context) error {
	c.mu.Lock()
	if c.closeCalled {
		wait := c.closeDone
		c.mu.Unlock()
		select {
		case <-wait:
			c.mu.Lock()
			result := c.closeResult
			c.mu.Unlock()
			return result
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.closeCalled = true
	connected := c.connected && c.terminal == nil
	c.connected = false
	c.stream = false
	c.mu.Unlock()
	var result error
	if connected {
		if disconnect, ok := c.sender.(disconnecter); ok {
			result = disconnect.Disconnect(ctx)
		}
	}
	if closer, ok := c.sender.(interface{ Close() error }); ok {
		result = errors.Join(result, closer.Close())
	}
	c.mu.Lock()
	c.closeResult = result
	c.mu.Unlock()
	c.once.Do(func() { close(c.done) })
	close(c.closeDone)
	return result
}

func (c *QueueClient) remoteClose() {
	c.mu.Lock()
	if c.closeCalled {
		c.mu.Unlock()
		return
	}
	c.closeCalled = true
	c.connected = false
	c.stream = false
	c.mu.Unlock()
	var result error
	if closer, ok := c.sender.(interface{ Close() error }); ok {
		result = closer.Close()
	}
	c.mu.Lock()
	c.closeResult = result
	c.mu.Unlock()
	c.once.Do(func() { close(c.done) })
	close(c.closeDone)
}
