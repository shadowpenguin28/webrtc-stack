package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func writeJSON(conn *websocket.Conn, message any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	return wsjson.Write(ctx, conn, message)
}

// clientRole tracks what role this WebSocket connection has registered as.
type clientRole int

const (
	roleUnknown clientRole = iota
	roleViewer
	roleNode
)

func webSocketHandler(hub *Hub, config Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// accept connection
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: config.AllowedOrigins})
		if err != nil {
			log.Printf("Websocket upgrade failed from %s: %v", r.RemoteAddr, err)
			return
		}

		role := roleUnknown
		clientID := "unregistered" // viewerId or nodeId depending on role

		defer func() {
			switch role {
			case roleViewer:
				released := hub.RemoveViewer(clientID)
				// Notify each node whose lease was released to tear down its peer connection
				for _, nodeID := range released {
					disconnect := SignalMessage{
						Type:   "disconnect",
						NodeID: nodeID,
					}
					if err := hub.RouteToNode(nodeID, disconnect); err != nil {
						log.Printf("ws: could not notify node %s of viewer disconnect: %v", nodeID, err)
					}
				}
			case roleNode:
				hub.RemoveNode(clientID)
			}
			log.Printf("ws: disconnected: role=%d id=%s remote=%s", role, clientID, r.RemoteAddr)
			_ = conn.CloseNow()
		}()

		conn.SetReadLimit(1 << 20)
		log.Printf("ws: connected from: %s", r.RemoteAddr)

		ctx := context.Background()
		for {
			var message SignalMessage

			err := wsjson.Read(ctx, conn, &message)
			if err != nil {
				log.Printf("ws: read error: %s: %v", r.RemoteAddr, err)
				return
			}

			switch message.Type {

			// ── Registration ──────────────────────────────────────────────

			case "register-viewer":
				if message.ViewerID == "" {
					sendError(conn, "", "viewerId is required")
					continue
				}
				role = roleViewer
				clientID = message.ViewerID
				hub.RegisterViewer(message.ViewerID, conn)
				log.Printf("ws: viewer registered: viewerId=%s remote=%s", message.ViewerID, r.RemoteAddr)

			case "register-node":
				if message.NodeID == "" {
					sendError(conn, "", "nodeId is required")
					continue
				}
				role = roleNode
				clientID = message.NodeID
				hub.RegisterNode(message.NodeID, message.Label, conn)
				log.Printf("ws: node registered: nodeId=%s label=%q remote=%s", message.NodeID, message.Label, r.RemoteAddr)

			// ── Node list ─────────────────────────────────────────────────

			case "list-nodes":
				response := NodeListMessage{
					Type:  "nodes",
					Nodes: hub.ListNodes(),
				}
				if err := writeJSON(conn, response); err != nil {
					log.Printf("ws: could not send node list to %s: %v", r.RemoteAddr, err)
					return
				}

			// ── WebRTC signaling: browser → node ──────────────────────────

			case "offer":
				if message.NodeID == "" || message.SessionID == "" {
					sendError(conn, message.NodeID, "nodeId and sessionId are required")
					continue
				}
				// Acquire exclusive lease
				if err := hub.AcquireLease(message.NodeID, clientID, message.SessionID); err != nil {
					sendError(conn, message.NodeID, err.Error())
					continue
				}
				// Route the offer to the node
				if err := hub.RouteToNode(message.NodeID, message); err != nil {
					hub.ReleaseLease(message.NodeID, message.SessionID)
					sendError(conn, message.NodeID, err.Error())
					continue
				}
				log.Printf("ws: offer routed: nodeId=%s sessionId=%s", message.NodeID, message.SessionID)

			// ── WebRTC signaling: node → browser ──────────────────────────

			case "answer":
				if message.NodeID == "" || message.SessionID == "" {
					sendError(conn, message.NodeID, "nodeId and sessionId are required")
					continue
				}
				viewerID := hub.LeaseOwner(message.NodeID, message.SessionID)
				if viewerID == "" {
					sendError(conn, message.NodeID, "no active lease for this session")
					continue
				}
				if err := hub.RouteToViewer(viewerID, message); err != nil {
					log.Printf("ws: could not route answer to viewer %s: %v", viewerID, err)
					continue
				}
				log.Printf("ws: answer routed: nodeId=%s sessionId=%s → viewerId=%s", message.NodeID, message.SessionID, viewerID)

			// ── ICE candidates: bidirectional ─────────────────────────────

			case "ice-candidate":
				if message.NodeID == "" || message.SessionID == "" {
					sendError(conn, message.NodeID, "nodeId and sessionId are required")
					continue
				}

				switch role {
				case roleViewer:
					// Browser → node
					if err := hub.RouteToNode(message.NodeID, message); err != nil {
						log.Printf("ws: could not route ICE candidate to node %s: %v", message.NodeID, err)
					}
				case roleNode:
					// Node → browser
					viewerID := hub.LeaseOwner(message.NodeID, message.SessionID)
					if viewerID == "" {
						continue
					}
					if err := hub.RouteToViewer(viewerID, message); err != nil {
						log.Printf("ws: could not route ICE candidate to viewer %s: %v", viewerID, err)
					}
				default:
					sendError(conn, message.NodeID, "must register before sending ICE candidates")
				}

			// ── Disconnect ────────────────────────────────────────────────

			case "disconnect":
				if message.NodeID == "" || message.SessionID == "" {
					sendError(conn, message.NodeID, "nodeId and sessionId are required")
					continue
				}
				hub.ReleaseLease(message.NodeID, message.SessionID)
				// Notify node to tear down its peer connection
				disconnect := SignalMessage{
					Type:      "disconnect",
					NodeID:    message.NodeID,
					SessionID: message.SessionID,
				}
				if err := hub.RouteToNode(message.NodeID, disconnect); err != nil {
					log.Printf("ws: could not send disconnect to node %s: %v", message.NodeID, err)
				}
				log.Printf("ws: disconnect: nodeId=%s sessionId=%s", message.NodeID, message.SessionID)

			// ── Health reports ─────────────────────────────────────────────

			case "node-health":
				// Log and store; future: forward to interested viewers
				log.Printf("ws: node-health: nodeId=%s health=%+v", message.NodeID, message.Health)

			// ── Unknown ───────────────────────────────────────────────────

			default:
				sendError(conn, message.NodeID, "unsupported message type: "+message.Type)
			}
		}
	})
}

// sendError writes a typed error message back to the client.
func sendError(conn *websocket.Conn, nodeID, message string) {
	response := SignalMessage{
		Type:    "error",
		NodeID:  nodeID,
		Message: message,
	}
	if err := writeJSON(conn, response); err != nil {
		log.Printf("ws: could not send error to client: %v", err)
	}
}
