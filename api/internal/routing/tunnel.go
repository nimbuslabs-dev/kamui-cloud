// Package routing implements the authenticated remote service transport:
// workers dial the coordinator outbound (no inbound ports), and the
// coordinator multiplexes proxied HTTP/WebSocket streams for live,
// assignment-fenced routes over that tunnel.
package routing

import (
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
	idMu    sync.Mutex

	lastPing time.Time

	closed   bool
	closedMu sync.Mutex
	lastPong time.Time
	pongMu   sync.Mutex
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
	if t.isClosed() {
		return nil, ErrTunnelClosed
	}
	if t.streamCount() >= MaxStreams {
		return nil, errors.New("tunnel is at its concurrent stream limit")
	}
	t.idMu.Lock()
	t.nextID++
	id := t.nextID
	t.idMu.Unlock()
	payload, err := json.Marshal(OpenFrame{
		StreamID: id, WorkloadID: workloadID, Fence: fence, Port: port,
	})
	if err != nil {
		return nil, err
	}
	pump := newPump(id)
	t.streams.Store(id, pump)
	if err := t.writeFrame(FrameOpen, id, payload); err != nil {
		t.streams.Delete(id)
		return nil, fmt.Errorf("tunnel dial: %w", err)
	}
	// The worker answers the dial with data (connected) or CLOSE (rejected).
	select {
	case first := <-pump.ch:
		pump.buffered = append(pump.buffered, first...)
		return &tunnelConn{pump: pump, tunnel: t}, nil
	case err := <-pump.errCh:
		t.streams.Delete(id)
		if err != nil {
			return nil, fmt.Errorf("tunnel dial rejected: %w", err)
		}
		return nil, errors.New("tunnel dial rejected by worker")
	case <-time.After(DialTimeout):
		t.streams.Delete(id)
		t.sendClose(id, "dial timeout")
		return nil, errors.New("tunnel dial timed out")
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
	lastPingSent := time.Now()
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
		t.pongMu.Lock()
		t.lastPong = time.Now()
		t.pongMu.Unlock()
		switch kind {
		case FrameData:
			if pump, ok := t.streams.Load(streamID); ok {
				pump.(*streamPump).deliver(payload)
			}
		case FrameEOF:
			if pump, ok := t.streams.Load(streamID); ok {
				pump.(*streamPump).closeWrite()
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
		if time.Since(lastPingSent) >= PingInterval {
			lastPingSent = time.Now()
			_ = t.writeFrame(FramePing, 0, nil)
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
	id       uint32
	ch       chan []byte
	errCh    chan error
	buffered []byte
	eofRead  bool
	readMu   sync.Mutex
	once     sync.Once
}

func newPump(id uint32) *streamPump {
	return &streamPump{
		id:    id,
		ch:    make(chan []byte, 32),
		errCh: make(chan error, 2),
	}
}

func (p *streamPump) deliver(payload []byte) {
	select {
	case p.ch <- payload:
	default:
		// Backpressure: the reader is slower than the worker; drop the
		// stream rather than buffering unbounded.
		p.shutdown(errors.New("stream buffer overflow"))
	}
}

func (p *streamPump) closeWrite() {
	p.readMu.Lock()
	p.eofRead = true
	p.readMu.Unlock()
}

func (p *streamPump) shutdown(err error) {
	p.once.Do(func() {
		if err != nil {
			select {
			case p.errCh <- err:
			default:
			}
		}
		close(p.ch)
	})
}

// tunnelConn adapts one tunnel stream to net.Conn for http.Transport.
type tunnelConn struct {
	pump   *streamPump
	tunnel *Tunnel
	readMu sync.Mutex
	closed bool
}

func (c *tunnelConn) Read(buf []byte) (int, error) {
	c.readMu.Lock()
	closed := c.closed
	c.readMu.Unlock()
	if closed {
		return 0, ErrTunnelClosed
	}
	if len(c.pump.buffered) > 0 {
		n := copy(buf, c.pump.buffered)
		c.pump.buffered = c.pump.buffered[n:]
		return n, nil
	}
	p := c.pump
	select {
	case payload, ok := <-p.ch:
		if !ok {
			p.readMu.Lock()
			eof := p.eofRead
			p.readMu.Unlock()
			if eof {
				return 0, io.EOF
			}
			return 0, ErrTunnelClosed
		}
		n := copy(buf, payload)
		return n, nil
	case err := <-p.errCh:
		return 0, err
	}
}

func (c *tunnelConn) Write(buf []byte) (int, error) {
	c.readMu.Lock()
	closed := c.closed
	c.readMu.Unlock()
	if closed {
		return 0, ErrTunnelClosed
	}
	for len(buf) > MaxFrameBytes {
		if err := c.tunnel.writeFrame(FrameData, c.pump.id, buf[:MaxFrameBytes]); err != nil {
			return 0, err
		}
		buf = buf[MaxFrameBytes:]
	}
	if err := c.tunnel.writeFrame(FrameData, c.pump.id, buf); err != nil {
		return 0, err
	}
	return len(buf), nil
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
	c.pump.shutdown(nil)
	return nil
}

func (c *tunnelConn) LocalAddr() net.Addr  { return pipeAddr("tunnel-local") }
func (c *tunnelConn) RemoteAddr() net.Addr { return pipeAddr("tunnel-worker:" + c.tunnel.WorkerID) }
func (c *tunnelConn) SetDeadline(t time.Time) error {
	c.tunnel.conn.SetWriteDeadline(t)
	return nil
}
func (c *tunnelConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *tunnelConn) SetWriteDeadline(t time.Time) error { return nil }

type pipeAddr string

func (a pipeAddr) Network() string { return "kamui-tunnel" }
func (a pipeAddr) String() string  { return string(a) }

// NewTunnel creates a tunnel for a live, upgraded worker connection.
func NewTunnel(workerID string, conn net.Conn, onRemove func()) *Tunnel {
	return &Tunnel{
		WorkerID: workerID,
		conn:     conn,
		lastPong: time.Now(),
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
