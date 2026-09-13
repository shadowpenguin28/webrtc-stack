package main

import (
	"encoding/json"
)

type SignalMessage struct {
	Type      string          `json:"type"`
	ViewerID  string          `json:"viewerId,omitempty"`
	NodeID    string          `json:"nodeId,omitempty"`
	Label     string          `json:"label,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Message   string          `json:"message,omitempty"`
	Node      *NodeInfo       `json:"node,omitempty"`
	Health    *NodeHealth     `json:"health,omitempty"`
}

type NodeInfo struct {
	NodeID string `json:"nodeId"`
	Label  string `json:"label"`
	Status string `json:"status"`
}

type NodeHealth struct {
	FFmpeg       string  `json:"ffmpeg,omitempty"`
	LastFrameAt  string  `json:"lastFrameAt,omitempty"`
	TemperatureC float64 `json:"temperatureC,omitempty"`
}

type NodeListMessage struct {
	Type  string     `json:"type"`
	Nodes []NodeInfo `json:"nodes"`
}