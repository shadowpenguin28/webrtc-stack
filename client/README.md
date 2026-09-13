# Base-station WebRTC client

This React application is the base-station video monitor. It receives one direct WebRTC video connection per rover node; it never receives media from the signaling server.

## Run

```bash
npm start
```

By default the client connects to `ws://<current-host>/ws`. Override it for development:

```bash
REACT_APP_SIGNALING_URL=ws://192.168.10.10:8080/ws npm start
```

For a one-off test, append `?signal=ws://host:port/ws` to the browser URL.

## Signaling contract

The browser is the WebRTC offerer and the rover node is the answerer. All messages are JSON over WebSocket. The Go signaling server routes `offer`, `answer`, and `ice-candidate` between the viewer and the node identified by `nodeId`, without modifying `payload`.

### Browser to server

```json
{ "type": "register-viewer", "viewerId": "persistent-browser-uuid" }
{ "type": "list-nodes" }
{ "type": "offer", "nodeId": "pi-a", "sessionId": "uuid", "payload": { "type": "offer", "sdp": "..." } }
{ "type": "ice-candidate", "nodeId": "pi-a", "sessionId": "uuid", "payload": { "candidate": "...", "sdpMid": "0", "sdpMLineIndex": 0 } }
{ "type": "disconnect", "nodeId": "pi-a", "sessionId": "uuid" }
```

### Server to browser

```json
{ "type": "nodes", "nodes": [{ "nodeId": "pi-a", "label": "Camera node A", "status": "online" }] }
{ "type": "node-status", "nodeId": "pi-a", "node": { "label": "Camera node A", "status": "online" } }
{ "type": "answer", "nodeId": "pi-a", "sessionId": "uuid", "payload": { "type": "answer", "sdp": "..." } }
{ "type": "ice-candidate", "nodeId": "pi-a", "sessionId": "uuid", "payload": { "candidate": "...", "sdpMid": "0", "sdpMLineIndex": 0 } }
{ "type": "error", "nodeId": "pi-a", "message": "Human-readable error" }
```

The browser creates a fresh `sessionId` for every requested stream. Preserve it while forwarding messages so late messages from an old connection cannot alter a new one.

## Debugging

The operator log shows signaling and WebRTC state changes. For candidate pairs, packet loss, bitrate, and decoded-frame statistics, open `chrome://webrtc-internals` in Chrome on the base station.
