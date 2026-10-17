package registry

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

var ErrNotFound = errors.New("node not found")

type NodeInfo struct {
	Hostname string   `json:"hostname"`
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
	CPUCores int      `json:"cpu_cores"`
	RAMMB    int      `json:"ram_mb"`
	GPU      string   `json:"gpu,omitempty"`
	IP       string   `json:"ip"`
	Roles    []string `json:"roles,omitempty"`
}

type Load struct {
	CPUPct *float64 `json:"cpu_pct,omitempty"`
	MemPct *float64 `json:"mem_pct,omitempty"`
}

type Node struct {
	NodeInfo
	ID       string `json:"id"`
	Status   string `json:"status"`
	LastSeen string `json:"last_seen"`
}

type storedNode struct {
	info     NodeInfo
	id       string
	lastSeen time.Time
	lastLoad *Load
}

type Registry struct {
	mu    sync.RWMutex
	nodes map[string]*storedNode
	Now   func() time.Time
}

func NewRegistry() *Registry {
	return &Registry{
		nodes: make(map[string]*storedNode),
		Now:   time.Now,
	}
}

func (r *Registry) generateID() string {
	for {
		var b [2]byte
		_, _ = rand.Read(b[:])
		id := "n-" + hex.EncodeToString(b[:])
		if _, exists := r.nodes[id]; !exists {
			return id
		}
	}
}

func (r *Registry) Register(info NodeInfo) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, n := range r.nodes {
		if n.info.Hostname == info.Hostname && n.info.IP == info.IP {
			n.info = info
			n.lastSeen = r.Now()
			return n.id
		}
	}

	id := r.generateID()
	r.nodes[id] = &storedNode{
		info:     info,
		id:       id,
		lastSeen: r.Now(),
	}
	return id
}

func (r *Registry) Heartbeat(id string, load *Load) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	node, exists := r.nodes[id]
	if !exists {
		return ErrNotFound
	}
	node.lastSeen = r.Now()
	if load != nil {
		node.lastLoad = load
	}
	return nil
}

func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.nodes[id]; !exists {
		return ErrNotFound
	}
	delete(r.nodes, id)
	return nil
}

func (r *Registry) Exists(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	_, exists := r.nodes[id]
	return exists
}

func (r *Registry) List() []Node {
	r.mu.RLock()
	defer r.mu.RUnlock()

	nodes := make([]Node, 0, len(r.nodes))
	now := r.Now()
	for _, n := range r.nodes {
		status := "offline"
		if now.Sub(n.lastSeen) < 15*time.Second {
			status = "online"
		}
		nodes = append(nodes, Node{
			NodeInfo: n.info,
			ID:       n.id,
			Status:   status,
			LastSeen: n.lastSeen.UTC().Format(time.RFC3339),
		})
	}

	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Hostname != nodes[j].Hostname {
			return nodes[i].Hostname < nodes[j].Hostname
		}
		return nodes[i].ID < nodes[j].ID
	})

	return nodes
}
