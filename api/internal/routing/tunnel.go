// Package routing implements the authenticated remote service transport:
// workers dial the coordinator outbound (no inbound ports), and the
// coordinator multiplexes proxied HTTP/WebSocket streams for live,
// assignment-fenced routes over that tunnel.
package routing

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	FrameOpen  byte = 0x01 // coordinator -> worker: dial workload:port
	FrameData  byte = 0x02 // both directions: stream payload
	FrameEOF   byte = 0x03 // both: half-close (writer finished)
	FrameClose byte = 0x04 // both: stream closed
	FramePing  byte = 0x05 // coordinator -> worker keepalive
	FramePong  byte = 0x06 // worker -> coordinator keepalive

	headerSize = 9 // type(1) + stream(4) + len(4)

	MaxFrameBytes = 1 << 20
	MaxStreams    = 64
	DialTimeout   = 10 * time.Second
	PingInterval  = 20 * time.Second
	PongTimeout   = 45 * time.Second
)

var ErrTunnelClosed = errors.New("tunnel is closed")

// OpenFrame is the coordinator's dial instruction for one proxied stream.
type OpenFrame struct {
	StreamID   uint32 `json:"stream_id"`
	WorkloadID string `json:"workload_id"`
	Fence      string `json:"fence"`
	Port       int    `json:"port"`
}

// CloseFrame terminates a stream with an optional diagnostic.
type CloseFrame struct {
	StreamID uint32 `json:"stream_id"`
	Reason   string `json:"reason,omitempty"`
}

// Tunnel is one authenticated worker control connection carrying multiplexed
// raw-byte streams.
type Tunnel struct {
	WorkerID string
	conn     net.Conn

	writeMu sync.Mutex
	streams sync.Map // streamID -> *streamPump
	nextID  uint32

	closed   bool
	closedMu sync.Mutex
	onRemove func()
}

func (t *Tunnel) isClosed() bool {
	t.closedMu.Lock()
	defer t.closedMu.Unlock()
	return t.closed
}

// writeFrame serializes one whole frame under the write lock.
func (t *Tunnel) writeFrame(kind byte, streamID uint32, payload []byte) error {
	header := make([]byte, headerSize)
	header[0] = kind
	binary.BigEndian.PutUint32(header[1:5], streamID)
	binary.BigEndian.PutUint32(header[5:9], uint32(len(payload)))
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	t.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := t.conn.Write(header); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := t.conn.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tunnel) streamCount() int {
	count := 0
	t.streams.Range(func(_, _ any) bool { count++; return true })
	return count
}

// OpenStream dials the route's container through the tunnel and returns a
// raw connection. The worker validates the assignment before dialing, so the
// coordinator can never direct a stream at an unauthorized destination.
func (t *Tunnel) OpenStream(workloadID, fence string, port int) (net.Conn, error) {
	return t.DialContext(context.Background(), workloadID, fence, port)
}

func (t *Tunnel) DialContext(ctx context.Context, workloadID, fence string, port int) (net.Conn, error) {
	t.closedMu.Lock()
	if t.closed {
		t.closedMu.Unlock()
		return nil, ErrTunnelClosed
	}
	if t.streamCount() >= MaxStreams {
		t.closedMu.Unlock()
		return nil, errors.New("tunnel is at its concurrent stream limit")
	}
	t.nextID++
	id := t.nextID
	pump := newPump(id)
	t.streams.Store(id, pump)
	t.closedMu.Unlock()
	payload, _ := json.Marshal(OpenFrame{StreamID: id, WorkloadID: workloadID, Fence: fence, Port: port})
	if err := t.writeFrame(FrameOpen, id, payload); err != nil {
		t.streams.Delete(id)
		t.remove()
		return nil, fmt.Errorf("tunnel dial: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()
	select {
	case first, ok := <-pump.ch:
		if ok {
			return &tunnelConn{pump: pump, tunnel: t, buffered: first}, nil
		}
		t.streams.Delete(id)
		return nil, errors.New("tunnel dial rejected by worker")
	case <-ctx.Done():
		t.streams.Delete(id)
		pump.shutdown(ctx.Err())
		t.sendClose(id, "dial cancelled")
		return nil, ctx.Err()
	}
}

func (t *Tunnel) sendClose(id uint32, reason string) {
	payload, _ := json.Marshal(CloseFrame{StreamID: id, Reason: reason})
	_ = t.writeFrame(FrameClose, id, payload)
}

// serve reads frames from the worker until the connection dies.
func (t *Tunnel) Serve() {
	defer t.remove()
	header := make([]byte, headerSize)
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(PingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if t.writeFrame(FramePing, 0, nil) != nil {
					t.remove()
					return
				}
			}
		}
	}()
	for {
		t.conn.SetReadDeadline(time.Now().Add(PongTimeout))
		if _, err := io.ReadFull(t.conn, header); err != nil {
			return
		}
		kind := header[0]
		streamID := binary.BigEndian.Uint32(header[1:5])
		length := binary.BigEndian.Uint32(header[5:9])
		if length > MaxFrameBytes {
			return
		}
		var payload []byte
		if length > 0 {
			payload = make([]byte, length)
			if _, err := io.ReadFull(t.conn, payload); err != nil {
				return
			}
		}
		switch kind {
		case FrameData:
			if pump, ok := t.streams.Load(streamID); ok {
				if !pump.(*streamPump).deliver(payload) {
					t.streams.Delete(streamID)
					t.sendClose(streamID, "receiver buffer full")
				}
			}
		case FrameEOF:
			if pump, ok := t.streams.Load(streamID); ok {
				pump.(*streamPump).shutdown(io.EOF)
			}
		case FrameClose:
			if pump, ok := t.streams.Load(streamID); ok {
				t.streams.Delete(streamID)
				pump.(*streamPump).shutdown(errors.New("worker closed the stream"))
			}
		case FramePong:
			// freshness recorded above
		case FramePing:
			_ = t.writeFrame(FramePong, 0, nil)
		default:
			return // protocol violation
		}

	}
}

func (t *Tunnel) remove() {
	t.closedMu.Lock()
	if t.closed {
		t.closedMu.Unlock()
		return
	}
	t.closed = true
	t.closedMu.Unlock()
	_ = t.conn.Close()
	if t.onRemove != nil {
		t.onRemove()
	}
	t.streams.Range(func(id, value any) bool {
		t.streams.Delete(id)
		value.(*streamPump).shutdown(ErrTunnelClosed)
		return true
	})
}

// streamPump moves bytes between the tunnel and one local pipe.
type streamPump struct {
	id  uint32
	ch  chan []byte
	mu  sync.Mutex
	err error
}

func newPump(id uint32) *streamPump {
	return &streamPump{id: id, ch: make(chan []byte, 32)}
}

func (p *streamPump) deliver(payload []byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return false
	}
	select {
	case p.ch <- payload:
		return true
	default:
		p.err = errors.New("stream buffer overflow")
		close(p.ch)
		return false
	}
}

func (p *streamPump) shutdown(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err == nil {
		p.err = err
		close(p.ch)
	}
}

// tunnelConn adapts one tunnel stream to net.Conn for http.Transport.
type tunnelConn struct {
	pump     *streamPump
	tunnel   *Tunnel
	readMu   sync.Mutex
	closed   bool
	buffered []byte
	bufferMu sync.Mutex
}

func (c *tunnelConn) Read(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	c.bufferMu.Lock()
	defer c.bufferMu.Unlock()
	for len(c.buffered) == 0 {
		payload, ok := <-c.pump.ch
		if !ok {
			c.pump.mu.Lock()
			defer c.pump.mu.Unlock()
			return 0, c.pump.err
		}
		c.buffered = payload
	}
	n := copy(buf, c.buffered)
	c.buffered = c.buffered[n:]
	return n, nil
}

func (c *tunnelConn) Write(buf []byte) (int, error) {
	c.readMu.Lock()
	closed := c.closed
	c.readMu.Unlock()
	if closed {
		return 0, ErrTunnelClosed
	}
	total := 0
	for len(buf) > 0 {
		size := min(len(buf), MaxFrameBytes)
		if err := c.tunnel.writeFrame(FrameData, c.pump.id, buf[:size]); err != nil {
			c.tunnel.remove()
			return total, err
		}
		total += size
		buf = buf[size:]
	}
	return total, nil
}

func (c *tunnelConn) Close() error {
	c.readMu.Lock()
	if c.closed {
		c.readMu.Unlock()
		return nil
	}
	c.closed = true
	c.readMu.Unlock()
	c.tunnel.streams.Delete(c.pump.id)
	c.tunnel.sendClose(c.pump.id, "closed by coordinator")
	c.pump.shutdown(ErrTunnelClosed)
	return nil
}

func (c *tunnelConn) LocalAddr() net.Addr  { return pipeAddr("tunnel-local") }
func (c *tunnelConn) RemoteAddr() net.Addr { return pipeAddr("tunnel-worker:" + c.tunnel.WorkerID) }
func (c *tunnelConn) SetDeadline(t time.Time) error {
	return errors.ErrUnsupported
}
func (c *tunnelConn) SetReadDeadline(t time.Time) error  { return errors.ErrUnsupported }
func (c *tunnelConn) SetWriteDeadline(t time.Time) error { return errors.ErrUnsupported }

type pipeAddr string

func (a pipeAddr) Network() string { return "kamui-tunnel" }
func (a pipeAddr) String() string  { return string(a) }

// NewTunnel creates a tunnel for a live, upgraded worker connection.
func NewTunnel(workerID string, conn net.Conn, onRemove func()) *Tunnel {
	return &Tunnel{
		WorkerID: workerID,
		conn:     conn,
		onRemove: onRemove,
	}
}

// Hub tracks live tunnels by worker ID.
type Hub struct {
	mu      sync.Mutex
	tunnels map[string]*Tunnel
}

func NewHub() *Hub {
	return &Hub{tunnels: make(map[string]*Tunnel)}
}

// Register installs a live tunnel, replacing any previous one.
func (h *Hub) Register(tunnel *Tunnel) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if previous, ok := h.tunnels[tunnel.WorkerID]; ok {
		go previous.remove()
	}
	h.tunnels[tunnel.WorkerID] = tunnel
}

// Remove drops a tunnel if it is still the registered one.
func (h *Hub) Remove(workerID string, tunnel *Tunnel) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if current, ok := h.tunnels[workerID]; ok && current == tunnel {
		delete(h.tunnels, workerID)
	}
}

// Get returns the live tunnel for a worker, or nil.
func (h *Hub) Get(workerID string) *Tunnel {
	h.mu.Lock()
	defer h.mu.Unlock()
	tunnel := h.tunnels[workerID]
	if tunnel != nil && tunnel.isClosed() {
		return nil
	}
	return tunnel
}

// Disconnect closes the current tunnel and all of its streams.
func (h *Hub) Disconnect(workerID string) {
	if tunnel := h.Get(workerID); tunnel != nil {
		tunnel.remove()
	}
}
