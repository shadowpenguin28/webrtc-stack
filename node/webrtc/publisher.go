// Package rtcpub provides a Pion-based WebRTC publisher that creates
// a PeerConnection with an H.264-only MediaEngine and exposes methods
// for handling offers, ICE candidates, and writing video samples.
package rtcpub

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// ICECandidate is a JSON-serialisable ICE candidate for signaling.
type ICECandidate struct {
	Candidate     string  `json:"candidate"`
	SDPMid        *string `json:"sdpMid,omitempty"`
	SDPMLineIndex *uint16 `json:"sdpMLineIndex,omitempty"`
}

// Publisher manages a single Pion PeerConnection that publishes one
// H.264 video track.
type Publisher struct {
	mu    sync.Mutex
	api   *webrtc.API
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticSample

	onICECandidate func(candidate ICECandidate)
	onDisconnect   func()

	udpPortMin uint16
	udpPortMax uint16
}

// Config configures the Publisher.
type Config struct {
	UDPPortMin uint16
	UDPPortMax uint16
}

// NewPublisher creates a Publisher with an H.264-only MediaEngine.
func NewPublisher(cfg Config) (*Publisher, error) {
	// H.264-only MediaEngine — the node will not negotiate VP8/VP9
	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			RTCPFeedback: []webrtc.RTCPFeedback{
				{Type: "nack"},
				{Type: "nack", Parameter: "pli"},
			},
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, fmt.Errorf("failed to register H.264 codec: %w", err)
	}

	se := &webrtc.SettingEngine{}
	if cfg.UDPPortMin > 0 && cfg.UDPPortMax > 0 {
		se.SetEphemeralUDPPortRange(cfg.UDPPortMin, cfg.UDPPortMax)
		log.Printf("rtcpub: UDP port range set to %d–%d", cfg.UDPPortMin, cfg.UDPPortMax)
	}

	i := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, i); err != nil {
		return nil, fmt.Errorf("failed to register default interceptors: %w", err)
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(me),
		webrtc.WithSettingEngine(*se),
		webrtc.WithInterceptorRegistry(i),
	)

	return &Publisher{
		api:        api,
		udpPortMin: cfg.UDPPortMin,
		udpPortMax: cfg.UDPPortMax,
	}, nil
}

// OnICECandidate registers a callback invoked for each trickle ICE candidate.
func (p *Publisher) OnICECandidate(fn func(candidate ICECandidate)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onICECandidate = fn
}

// OnDisconnect registers a callback invoked when the peer connection enters
// a failed or closed state.
func (p *Publisher) OnDisconnect(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onDisconnect = fn
}

// HandleOffer accepts an SDP offer from the browser, creates a video track,
// and returns an SDP answer. The offer payload is the raw JSON from signaling.
func (p *Publisher) HandleOffer(offerPayload json.RawMessage) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Close any existing peer connection
	if p.pc != nil {
		_ = p.pc.Close()
		p.pc = nil
		p.track = nil
	}

	// Create a new PeerConnection with no ICE servers (LAN-only)
	pc, err := p.api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("failed to create PeerConnection: %w", err)
	}

	// Create H.264 video track
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			RTCPFeedback: []webrtc.RTCPFeedback{
				{Type: "nack"},
				{Type: "nack", Parameter: "pli"},
			},
		},
		"video",      // track ID
		"rover-feed", // stream ID
	)
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("failed to create video track: %w", err)
	}

	if _, err := pc.AddTrack(track); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("failed to add track: %w", err)
	}

	// ICE candidate callback
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		p.mu.Lock()
		cb := p.onICECandidate
		p.mu.Unlock()

		if cb != nil {
			init := c.ToJSON()
			candidate := ICECandidate{
				Candidate:     init.Candidate,
				SDPMid:        init.SDPMid,
				SDPMLineIndex: init.SDPMLineIndex,
			}
			cb(candidate)
		}
	})

	// Connection state monitoring
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("rtcpub: connection state: %s", state)
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			p.mu.Lock()
			cb := p.onDisconnect
			p.mu.Unlock()
			if cb != nil {
				cb()
			}
		}
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("rtcpub: ICE state: %s", state)
	})

	// Parse and set the remote offer
	var offer webrtc.SessionDescription
	if err := json.Unmarshal(offerPayload, &offer); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("failed to parse offer SDP: %w", err)
	}

	if err := pc.SetRemoteDescription(offer); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("failed to set remote description: %w", err)
	}

	// Create and set the answer
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("failed to create answer: %w", err)
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("failed to set local description: %w", err)
	}

	p.pc = pc
	p.track = track

	// Marshal the answer for signaling
	answerJSON, err := json.Marshal(pc.LocalDescription())
	if err != nil {
		return nil, fmt.Errorf("failed to marshal answer: %w", err)
	}

	log.Printf("rtcpub: answer created")
	return answerJSON, nil
}

// AddICECandidate applies a remote ICE candidate from the browser.
func (p *Publisher) AddICECandidate(candidatePayload json.RawMessage) error {
	p.mu.Lock()
	pc := p.pc
	p.mu.Unlock()

	if pc == nil {
		return fmt.Errorf("no active PeerConnection")
	}

	var init webrtc.ICECandidateInit
	if err := json.Unmarshal(candidatePayload, &init); err != nil {
		return fmt.Errorf("failed to parse ICE candidate: %w", err)
	}

	return pc.AddICECandidate(init)
}

// WriteSample writes one complete H.264 access unit to the video track.
func (p *Publisher) WriteSample(data []byte, duration time.Duration) error {
	p.mu.Lock()
	track := p.track
	p.mu.Unlock()

	if track == nil {
		return fmt.Errorf("no active video track")
	}

	return track.WriteSample(media.Sample{
		Data:     data,
		Duration: duration,
	})
}

// HasActiveConnection returns true if a PeerConnection is currently established.
func (p *Publisher) HasActiveConnection() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pc != nil
}

// Close tears down the current PeerConnection if any.
func (p *Publisher) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.pc != nil {
		_ = p.pc.Close()
		p.pc = nil
		p.track = nil
		log.Printf("rtcpub: PeerConnection closed")
	}
}
