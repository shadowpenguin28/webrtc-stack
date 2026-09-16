package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"rovernode/pipeline"
	"rovernode/signaling"
	rtcpub "rovernode/webrtc"
)

func main() {
	config, err := LoadConfig("config.toml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	log.Printf("Loaded node config: nodeId=%s label=%q type=%s signaling=%s",
		config.NodeID, config.Label, config.NodeType, config.SignalingURL)

	// Top-level context for graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Create the WebRTC publisher
	publisher, err := rtcpub.NewPublisher(rtcpub.Config{
		UDPPortMin: config.WebRTC.UDPPortMin,
		UDPPortMax: config.WebRTC.UDPPortMax,
	})
	if err != nil {
		log.Fatalf("Failed to create WebRTC publisher: %v", err)
	}
	defer publisher.Close()

	// Create the signaling client
	sigClient := signaling.NewClient(config.SignalingURL, config.NodeID, config.Label)
	defer sigClient.Close()

	// Start signaling connection in background
	go sigClient.Connect()

	// Main session loop: wait for offers, handle WebRTC, drive video pipeline
	sessionLoop(ctx, config, sigClient, publisher)

	log.Printf("Node shutting down")
}

// sessionLoop handles one WebRTC session at a time: waits for an offer,
// creates a PeerConnection, starts the video pipeline, and tears everything
// down on disconnect or shutdown.
func sessionLoop(ctx context.Context, config Config, sigClient *signaling.Client, publisher *rtcpub.Publisher) {
	for {
		select {
		case <-ctx.Done():
			return

		case offer := <-sigClient.Offers():
			log.Printf("session: received offer sessionId=%s", offer.SessionID)
			handleSession(ctx, config, sigClient, publisher, offer)
		}
	}
}

// handleSession manages one complete WebRTC session lifecycle.
func handleSession(ctx context.Context, config Config, sigClient *signaling.Client, publisher *rtcpub.Publisher, offer signaling.Message) {
	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()

	sessionID := offer.SessionID

	// Set up ICE candidate forwarding from publisher → signaling
	publisher.OnICECandidate(func(candidate rtcpub.ICECandidate) {
		candidateJSON, err := json.Marshal(candidate)
		if err != nil {
			log.Printf("session: failed to marshal ICE candidate: %v", err)
			return
		}
		msg := signaling.Message{
			Type:      "ice-candidate",
			NodeID:    config.NodeID,
			SessionID: sessionID,
			Payload:   candidateJSON,
		}
		if err := sigClient.SendICECandidate(msg); err != nil {
			log.Printf("session: failed to send ICE candidate: %v", err)
		}
	})

	// Tear down on peer disconnect
	publisher.OnDisconnect(func() {
		log.Printf("session: peer disconnected, ending session")
		sessionCancel()
	})

	// Handle the offer → create answer
	answerPayload, err := publisher.HandleOffer(offer.Payload)
	if err != nil {
		log.Printf("session: failed to handle offer: %v", err)
		return
	}

	// Send answer back through signaling
	answerMsg := signaling.Message{
		Type:      "answer",
		NodeID:    config.NodeID,
		SessionID: sessionID,
		Payload:   answerPayload,
	}
	if err := sigClient.SendAnswer(answerMsg); err != nil {
		log.Printf("session: failed to send answer: %v", err)
		publisher.Close()
		return
	}
	log.Printf("session: answer sent")

	// Start consuming ICE candidates and disconnect signals in background
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-sessionCtx.Done():
				return
			case candidate := <-sigClient.ICECandidates():
				if candidate.SessionID != sessionID {
					continue
				}
				if err := publisher.AddICECandidate(candidate.Payload); err != nil {
					log.Printf("session: failed to add ICE candidate: %v", err)
				}
			case disconnect := <-sigClient.Disconnects():
				if disconnect.SessionID == "" || disconnect.SessionID == sessionID {
					log.Printf("session: received disconnect signal")
					sessionCancel()
					return
				}
			}
		}
	}()

	// Start the video pipeline
	var source pipeline.VideoSource
	if config.Stitch.Enabled && len(config.Stitch.Cameras) > 0 {
		// Multi-camera stitched mode
		stitchCfg := pipeline.StitchConfig{
			CanvasWidth:  config.Stitch.CanvasWidth,
			CanvasHeight: config.Stitch.CanvasHeight,
			Encoder:      config.Stitch.Encoder,
			FPS:          config.Video.FPS,
			Bitrate:      config.Stitch.Bitrate,
		}
		for _, cam := range config.Stitch.Cameras {
			stitchCfg.Cameras = append(stitchCfg.Cameras, pipeline.CameraInput{
				Device:      cam.Device,
				Label:       cam.Label,
				InputFormat: cam.InputFormat,
				Width:       cam.Width,
				Height:      cam.Height,
			})
		}
		source = pipeline.NewStitchedSource(stitchCfg)
		log.Printf("session: using stitched pipeline (%d cameras)", len(config.Stitch.Cameras))
	} else {
		// Single-camera mode
		source = pipeline.NewCameraSource(config.FFmpeg.Arguments, config.Video.FPS)
		log.Printf("session: using single-camera pipeline")
	}

	frameDuration := time.Second / time.Duration(config.Video.FPS)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := source.Start(sessionCtx); err != nil && sessionCtx.Err() == nil {
			log.Printf("session: video pipeline error: %v", err)
			sessionCancel()
		}
	}()

	// Pump frames from pipeline to WebRTC track
	for {
		select {
		case <-sessionCtx.Done():
			source.Stop()
			publisher.Close()
			wg.Wait()
			log.Printf("session: ended sessionId=%s", sessionID)
			return
		case frame := <-source.Frames():
			if err := publisher.WriteSample(frame.Data, frameDuration); err != nil {
				log.Printf("session: write sample error: %v", err)
			}
		}
	}
}
