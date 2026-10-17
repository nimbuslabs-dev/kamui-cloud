package registry

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestRegistryLifecycle(t *testing.T) {
	reg := NewRegistry()
	currentTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	reg.Now = func() time.Time {
		return currentTime
	}

	info := NodeInfo{
		Hostname: "test-node",
		OS:       "macos",
		Arch:     "arm64",
		CPUCores: 8,
		RAMMB:    16384,
		IP:       "192.168.1.50",
	}

	id := reg.Register(info)
	if id == "" {
		t.Fatalf("expected non-empty id")
	}

	nodes := reg.List()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	if nodes[0].ID != id {
		t.Fatalf("expected node id %q, got %q", id, nodes[0].ID)
	}
	if nodes[0].Status != "online" {
		t.Fatalf("expected online status, got %q", nodes[0].Status)
	}

	currentTime = currentTime.Add(15 * time.Second)
	nodes = reg.List()
	if nodes[0].Status != "offline" {
		t.Fatalf("expected offline status after 15s, got %q", nodes[0].Status)
	}

	cpu := 15.5
	mem := 42.0
	load := &Load{CPUPct: &cpu, MemPct: &mem}
	if err := reg.Heartbeat(id, load); err != nil {
		t.Fatalf("unexpected heartbeat error: %v", err)
	}

	nodes = reg.List()
	if nodes[0].Status != "online" {
		t.Fatalf("expected online status after heartbeat, got %q", nodes[0].Status)
	}

	if err := reg.Heartbeat("n-unknown", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for unknown id, got %v", err)
	}

	if err := reg.Remove(id); err != nil {
		t.Fatalf("unexpected remove error: %v", err)
	}

	if len(reg.List()) != 0 {
		t.Fatalf("expected 0 nodes after removal")
	}

	if err := reg.Remove(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound when removing already removed node, got %v", err)
	}
}

func TestRegistrySorting(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NodeInfo{Hostname: "node-c", OS: "linux", Arch: "amd64", CPUCores: 4, RAMMB: 8192, IP: "10.0.0.3"})
	reg.Register(NodeInfo{Hostname: "node-a", OS: "linux", Arch: "amd64", CPUCores: 4, RAMMB: 8192, IP: "10.0.0.1"})
	reg.Register(NodeInfo{Hostname: "node-b", OS: "linux", Arch: "amd64", CPUCores: 4, RAMMB: 8192, IP: "10.0.0.2"})
	reg.Register(NodeInfo{Hostname: "node-a", OS: "linux", Arch: "amd64", CPUCores: 4, RAMMB: 8192, IP: "10.0.0.4"})

	nodes := reg.List()
	if len(nodes) != 4 {
		t.Fatalf("expected 4 nodes, got %d", len(nodes))
	}

	for i := 0; i < len(nodes)-1; i++ {
		cur := nodes[i]
		next := nodes[i+1]
		if cur.Hostname > next.Hostname {
			t.Fatalf("nodes not sorted by hostname: %s > %s", cur.Hostname, next.Hostname)
		}
		if cur.Hostname == next.Hostname && cur.ID > next.ID {
			t.Fatalf("nodes with same hostname not sorted by id: %s > %s", cur.ID, next.ID)
		}
	}
}

func TestRegistryConcurrency(t *testing.T) {
	reg := NewRegistry()
	var wg sync.WaitGroup

	ids := make([]string, 10)
	for i := 0; i < 10; i++ {
		ids[i] = reg.Register(NodeInfo{
			Hostname: fmt.Sprintf("node-%d", i),
			OS:       "linux",
			Arch:     "amd64",
			CPUCores: 2,
			RAMMB:    2048,
			IP:       fmt.Sprintf("10.0.0.%d", i),
		})
	}

	for i := 0; i < 20; i++ {
		wg.Add(3)
		go func(idx int) {
			defer wg.Done()
			reg.Register(NodeInfo{
				Hostname: fmt.Sprintf("dynamic-%d", idx),
				OS:       "linux",
				Arch:     "amd64",
				CPUCores: 2,
				RAMMB:    2048,
				IP:       "10.0.1.1",
			})
		}(i)

		go func(idx int) {
			defer wg.Done()
			targetID := ids[idx%len(ids)]
			cpu := float64(idx)
			_ = reg.Heartbeat(targetID, &Load{CPUPct: &cpu})
		}(i)

		go func() {
			defer wg.Done()
			_ = reg.List()
		}()
	}

	wg.Wait()
}
