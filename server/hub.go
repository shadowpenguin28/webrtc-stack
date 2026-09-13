package main

import (
	"fmt"
	"log"
	"sync"

	"github.com/coder/websocket"
)

// ConnectedNode represents a rover node with an active WebSocket connection.
type ConnectedNode struct {
	NodeID string
	Label  string
	Conn   *websocket.Conn
}

// ConnectedViewer represents a browser viewer with an active WebSocket connection.
type ConnectedViewer struct {
	ViewerID string
	Conn     *websocket.Conn
}

// ViewerLease binds a node exclusively to one viewer+session.
// At most one lease exists per nodeId in the first release.
type ViewerLease struct {
	ViewerID  string
	SessionID string
}

// Hub coordinates all connected nodes, viewers, and their leases.
// All public methods are safe for concurrent use.
type Hub struct {
	mu      sync.RWMutex
	config  Config
	nodes   map[string]*ConnectedNode   // nodeId → node
	viewers map[string]*ConnectedViewer  // viewerId → viewer
	leases  map[string]*ViewerLease     // nodeId → lease
}

// NewHub creates a Hub with the given server configuration.
func NewHub(config Config) *Hub {
	return &Hub{
		config:  config,
		nodes:   make(map[string]*ConnectedNode),
		viewers: make(map[string]*ConnectedViewer),
		leases:  make(map[string]*ViewerLease),
	}
}

// RegisterNode adds a rover node and broadcasts its online status to all viewers.
func (h *Hub) RegisterNode(nodeID, label string, conn *websocket.Conn) {
	h.mu.Lock()
	h.nodes[nodeID] = &ConnectedNode{
		NodeID: nodeID,
		Label:  label,
		Conn:   conn,
	}
	// snapshot viewers for broadcast outside lock
	viewers := h.viewerSnapshot()
	h.mu.Unlock()

	log.Printf("hub: node registered: nodeId=%s label=%q", nodeID, label)

	status := SignalMessage{
		Type:   "node-status",
		NodeID: nodeID,
		Node: &NodeInfo{
			NodeID: nodeID,
			Label:  label,
			Status: "online",
		},
	}
	h.broadcastToViewers(viewers, status)
}

// RemoveNode removes a node, releases its lease, and broadcasts offline status.
func (h *Hub) RemoveNode(nodeID string) {
	h.mu.Lock()
	node, exists := h.nodes[nodeID]
	if !exists {
		h.mu.Unlock()
		return
	}
	delete(h.nodes, nodeID)
	delete(h.leases, nodeID)
	viewers := h.viewerSnapshot()
	h.mu.Unlock()

	log.Printf("hub: node removed: nodeId=%s label=%q", nodeID, node.Label)

	status := SignalMessage{
		Type:   "node-status",
		NodeID: nodeID,
		Node: &NodeInfo{
			NodeID: nodeID,
			Label:  node.Label,
			Status: "offline",
		},
	}
	h.broadcastToViewers(viewers, status)
}

// RegisterViewer adds a browser viewer.
func (h *Hub) RegisterViewer(viewerID string, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.viewers[viewerID] = &ConnectedViewer{
		ViewerID: viewerID,
		Conn:     conn,
	}
	log.Printf("hub: viewer registered: viewerId=%s", viewerID)
}

// RemoveViewer removes a viewer and releases all leases held by that viewer.
// Returns the list of nodeIds whose leases were released (so the caller can
// notify those nodes to tear down their peer connections).
func (h *Hub) RemoveViewer(viewerID string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.viewers, viewerID)

	var released []string
	for nodeID, lease := range h.leases {
		if lease.ViewerID == viewerID {
			delete(h.leases, nodeID)
			released = append(released, nodeID)
			log.Printf("hub: lease released on viewer disconnect: nodeId=%s viewerId=%s", nodeID, viewerID)
		}
	}

	log.Printf("hub: viewer removed: viewerId=%s releasedLeases=%d", viewerID, len(released))
	return released
}

// AcquireLease attempts to exclusively reserve a node for a viewer+session.
// Returns an error if the node is already leased to another session.
func (h *Hub) AcquireLease(nodeID, viewerID, sessionID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if existing, ok := h.leases[nodeID]; ok {
		if existing.SessionID != sessionID {
			return fmt.Errorf("Node is already being viewed")
		}
		// Same session re-requesting — idempotent
		return nil
	}

	h.leases[nodeID] = &ViewerLease{
		ViewerID:  viewerID,
		SessionID: sessionID,
	}
	log.Printf("hub: lease acquired: nodeId=%s viewerId=%s sessionId=%s", nodeID, viewerID, sessionID)
	return nil
}

// ReleaseLease frees the lease on a node if the sessionId matches.
func (h *Hub) ReleaseLease(nodeID, sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if existing, ok := h.leases[nodeID]; ok && existing.SessionID == sessionID {
		delete(h.leases, nodeID)
		log.Printf("hub: lease released: nodeId=%s sessionId=%s", nodeID, sessionID)
	}
}

// LeaseOwner returns the viewerId that holds the lease for a node, or "" if none.
func (h *Hub) LeaseOwner(nodeID, sessionID string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if lease, ok := h.leases[nodeID]; ok && lease.SessionID == sessionID {
		return lease.ViewerID
	}
	return ""
}

// RouteToNode sends a message to a connected node. Returns an error if the node
// is not connected.
func (h *Hub) RouteToNode(nodeID string, msg SignalMessage) error {
	h.mu.RLock()
	node, ok := h.nodes[nodeID]
	h.mu.RUnlock()

	if !ok {
		return fmt.Errorf("Node is offline")
	}

	if err := writeJSON(node.Conn, msg); err != nil {
		return fmt.Errorf("failed to send to node %s: %w", nodeID, err)
	}
	return nil
}

// RouteToViewer sends a message to a connected viewer. Returns an error if the
// viewer is not connected.
func (h *Hub) RouteToViewer(viewerID string, msg SignalMessage) error {
	h.mu.RLock()
	viewer, ok := h.viewers[viewerID]
	h.mu.RUnlock()

	if !ok {
		return fmt.Errorf("viewer %s is not connected", viewerID)
	}

	if err := writeJSON(viewer.Conn, msg); err != nil {
		return fmt.Errorf("failed to send to viewer %s: %w", viewerID, err)
	}
	return nil
}

// ListNodes returns a snapshot of all currently registered nodes.
func (h *Hub) ListNodes() []NodeInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()

	nodes := make([]NodeInfo, 0, len(h.nodes))
	for _, node := range h.nodes {
		nodes = append(nodes, NodeInfo{
			NodeID: node.NodeID,
			Label:  node.Label,
			Status: "online",
		})
	}
	return nodes
}

// viewerSnapshot returns the current set of viewer connections.
// Must be called with h.mu held (read or write).
func (h *Hub) viewerSnapshot() []*ConnectedViewer {
	viewers := make([]*ConnectedViewer, 0, len(h.viewers))
	for _, v := range h.viewers {
		viewers = append(viewers, v)
	}
	return viewers
}

// broadcastToViewers sends a message to a list of viewers.
// Errors are logged but do not stop the broadcast.
func (h *Hub) broadcastToViewers(viewers []*ConnectedViewer, msg SignalMessage) {
	for _, viewer := range viewers {
		if err := writeJSON(viewer.Conn, msg); err != nil {
			log.Printf("hub: broadcast to viewer %s failed: %v", viewer.ViewerID, err)
		}
	}
}
