// Package signaling provides a WebSocket client that connects to the
// base-station signaling service, registers as a rover node, and
// provides channels/methods for exchanging WebRTC signaling messages.
package signaling

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Message mirrors the signaling protocol JSON schema.
type Message struct {
	Type      string          `json:"type"`
	NodeID    string          `json:"nodeId,omitempty"`
	Label     string          `json:"label,omitempty"`
	ViewerID  string          `json:"viewerId,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Message   string          `json:"message,omitempty"`
}

// Client maintains a persistent WebSocket connection to the signaling
// service with automatic reconnection and bounded exponential backoff.
type Client struct {
	url    string
	nodeID string
	label  string

	mu   sync.Mutex
	conn *websocket.Conn

	offers         chan Message
	iceCandidates  chan Message
	disconnects    chan Message

	ctx    context.Context
	cancel context.CancelFunc
}

// NewClient creates a signaling client for the given node identity.
func NewClient(url, nodeID, label string) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		url:           url,
		nodeID:        nodeID,
		label:         label,
		offers:        make(chan Message, 4),
		iceCandidates: make(chan Message, 32),
		disconnects:   make(chan Message, 4),
		ctx:           ctx,
		cancel:        cancel,
	}
}

// Offers returns a channel that receives WebRTC offers from browsers.
func (c *Client) Offers() <-chan Message { return c.offers }

// ICECandidates returns a channel that receives ICE candidates from browsers.
func (c *Client) ICECandidates() <-chan Message { return c.iceCandidates }

// Disconnects returns a channel that receives disconnect notifications.
func (c *Client) Disconnects() <-chan Message { return c.disconnects }

// Connect starts the persistent connection loop. It blocks until the
// context is cancelled. Call this in a goroutine.
func (c *Client) Connect() {
	attempt := 0
	for {
		if c.ctx.Err() != nil {
			return
		}

		err := c.connectOnce()
		if c.ctx.Err() != nil {
			return
		}

		attempt++
		backoff := backoffDuration(attempt)
		log.Printf("signaling: connection lost: %v — reconnecting in %v", err, backoff)

		select {
		case <-time.After(backoff):
		case <-c.ctx.Done():
			return
		}
	}
}

// connectOnce establishes one WebSocket connection, registers, and reads
// messages until the connection breaks or the context is cancelled.
func (c *Client) connectOnce() error {
	log.Printf("signaling: connecting to %s as %s", c.url, c.nodeID)

	conn, _, err := websocket.Dial(c.ctx, c.url, nil)
	if err != nil {
		return err
	}
	conn.SetReadLimit(1 << 20)

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
		_ = conn.CloseNow()
	}()

	// Register with the signaling service
	reg := Message{
		Type:   "register-node",
		NodeID: c.nodeID,
		Label:  c.label,
	}
	if err := c.writeJSON(reg); err != nil {
		return err
	}
	log.Printf("signaling: registered as %s", c.nodeID)

	// Read loop
	for {
		var msg Message
		err := wsjson.Read(c.ctx, conn, &msg)
		if err != nil {
			return err
		}

		switch msg.Type {
		case "offer":
			select {
			case c.offers <- msg:
			default:
				log.Printf("signaling: offer channel full, dropping offer sessionId=%s", msg.SessionID)
			}
		case "ice-candidate":
			select {
			case c.iceCandidates <- msg:
			default:
				log.Printf("signaling: ICE candidate channel full, dropping")
			}
		case "disconnect":
			select {
			case c.disconnects <- msg:
			default:
				log.Printf("signaling: disconnect channel full, dropping")
			}
		case "error":
			log.Printf("signaling: server error: %s", msg.Message)
		default:
			log.Printf("signaling: ignoring message type: %s", msg.Type)
		}
	}
}

// SendAnswer sends an SDP answer back through the signaling service.
func (c *Client) SendAnswer(msg Message) error {
	return c.writeJSON(msg)
}

// SendICECandidate sends an ICE candidate back through the signaling service.
func (c *Client) SendICECandidate(msg Message) error {
	return c.writeJSON(msg)
}

// Close shuts down the signaling client.
func (c *Client) Close() {
	c.cancel()
	c.mu.Lock()
	if c.conn != nil {
		_ = c.conn.Close(websocket.StatusNormalClosure, "shutting down")
	}
	c.mu.Unlock()
}

// writeJSON sends a JSON message on the current connection.
func (c *Client) writeJSON(msg Message) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return context.Canceled
	}

	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()

	return wsjson.Write(ctx, conn, msg)
}

// backoffDuration returns an exponential backoff duration capped at 30 seconds.
func backoffDuration(attempt int) time.Duration {
	secs := math.Min(float64(int(1)<<uint(attempt)), 30)
	return time.Duration(secs) * time.Second
}
