package routing

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// pipeTunnels wires a coordinator tunnel to a fake worker peer over real
// sockets, exercising the actual frame codec.
func pipeTunnels(t *testing.T) (*Tunnel, net.Conn) {
	t.Helper()
	server, client := net.Pipe()
	tunnel := NewTunnel("wk-test", server, nil)
	go tunnel.Serve()
	return tunnel, client
}

func writeFrame(t *testing.T, conn net.Conn, kind byte, id uint32, payload []byte) {
	t.Helper()
	header := make([]byte, headerSize)
	header[0] = kind
	binary.BigEndian.PutUint32(header[1:5], id)
	binary.BigEndian.PutUint32(header[5:9], uint32(len(payload)))
	if _, err := conn.Write(header); err != nil {
		t.Fatal(err)
	}
	if len(payload) > 0 {
		if _, err := conn.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
}

func readFrame(t *testing.T, conn net.Conn) (byte, uint32, []byte) {
	t.Helper()
	header := make([]byte, headerSize)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Fatal(err)
	}
	length := binary.BigEndian.Uint32(header[5:9])
	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(conn, payload); err != nil {
			t.Fatal(err)
		}
	}
	return header[0], binary.BigEndian.Uint32(header[1:5]), payload
}

func TestTunnelStreamRoundTrip(t *testing.T) {
	tunnel, peer := pipeTunnels(t)
	defer tunnel.remove()
	defer peer.Close()

	var dialed atomic.Bool
	go func() {
		// Fake worker: answer the OPEN with data, echo one round trip, then EOF.
		kind, id, payload := readFrame(t, peer)
		if kind != FrameOpen {
			t.Errorf("expected OPEN, got %d", kind)
			return
		}
		var open OpenFrame
		if err := json.Unmarshal(payload, &open); err != nil {
			t.Errorf("decode OPEN: %v", err)
			return
		}
		if open.WorkloadID != "wl-1" || open.Port != 8080 {
			t.Errorf("open frame = %+v", open)
			return
		}
		dialed.Store(true)
		writeFrame(t, peer, FrameData, id, []byte("hello"))
		// Echo loop for the test's writes.
		for {
			kind, id, payload = readFrame(t, peer)
			switch kind {
			case FrameData:
				writeFrame(t, peer, FrameData, id, payload)
			case FrameClose:
				return
			}
		}
	}()

	conn, err := tunnel.OpenStream("wl-1", "fence-1", 8080)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer conn.Close()
	if !dialed.Load() {
		t.Fatal("worker never saw the OPEN")
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("first read = %q, %v", buf, err)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := io.ReadFull(conn, buf[:4]); err != nil || string(buf[:4]) != "ping" {
		t.Fatalf("echo read = %q, %v", buf[:4], err)
	}
}

func TestTunnelDialRejection(t *testing.T) {
	tunnel, peer := pipeTunnels(t)
	defer tunnel.remove()
	defer peer.Close()
	go func() {
		kind, id, payload := readFrame(t, peer)
		if kind != FrameOpen {
			return
		}
		_ = payload
		writeFrame(t, peer, FrameClose, id, []byte(`{"stream_id":0,"reason":"no such assignment"}`))

	}()
	_, err := tunnel.OpenStream("wl-unknown", "fence", 80)
	if err == nil {
		t.Fatal("rejected dial must surface an error")
	}
	if got := err.Error(); got == "" {
		t.Fatal("rejection reason must be preserved")
	}
}

func TestTunnelHubReplaceAndRemove(t *testing.T) {
	hub := NewHub()
	clientA, serverA := net.Pipe()
	clientB, serverB := net.Pipe()
	defer clientA.Close()
	defer clientB.Close()
	first := NewTunnel("wk-a", serverA, nil)
	second := NewTunnel("wk-a", serverB, nil)
	hub.Register(first)
	hub.Register(second)
	if hub.Get("wk-a") != second {
		t.Fatal("the latest tunnel must win")
	}
	hub.Remove("wk-a", second)
	if hub.Get("wk-a") != nil {
		t.Fatal("removed tunnel must be gone")
	}
}

func TestClosedTunnelFailsOpens(t *testing.T) {
	tunnel, peer := pipeTunnels(t)
	peer.Close()
	tunnel.remove()
	deadline := time.Now().Add(time.Second)
	for tunnel.isClosed() == false && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, err := tunnel.OpenStream("wl-x", "f", 80); err == nil {
		t.Fatal("closed tunnels must refuse new streams")
	}
}

func TestHTTPResponseAcrossWorkerTunnel(t *testing.T) {
	tunnel, peer := pipeTunnels(t)
	defer tunnel.remove()
	defer peer.Close()
	body := bytes.Repeat([]byte("worker-response "), 4096)
	go func() {
		kind, id, _ := readFrame(t, peer)
		if kind != FrameOpen {
			t.Error("missing worker OPEN")
			return
		}
		writeFrame(t, peer, FrameData, id, nil)
		kind, _, request := readFrame(t, peer)
		if kind != FrameData || !bytes.Contains(request, []byte("GET /v1/models HTTP/1.1")) {
			t.Errorf("unexpected upstream request %q", request)
			return
		}
		writeFrame(t, peer, FrameData, id, append([]byte("HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n"), body...))
		writeFrame(t, peer, FrameEOF, id, nil)
		readFrame(t, peer)
	}()
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return tunnel.DialContext(ctx, "wl-1", "fence-1", 8080)
		},
	}}
	resp, err := client.Get("http://kamui-service/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("response: %d/%d bytes, %v", len(got), len(body), err)
	}
}
