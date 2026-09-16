package main

import (
	"log"
	"os"

	"github.com/pelletier/go-toml/v2"
)

// Config is the top-level rover node configuration, loaded from TOML.
type Config struct {
	NodeID       string        `toml:"node_id"`
	Label        string        `toml:"label"`
	NodeType     string        `toml:"node_type"`
	SignalingURL string        `toml:"signaling_url"`
	WebRTC       WebRTCConfig  `toml:"webrtc"`
	Video        VideoConfig   `toml:"video"`
	FFmpeg       FFmpegConfig  `toml:"ffmpeg"`
	Stitch       StitchCfg     `toml:"stitch"`
}

// WebRTCConfig controls Pion's ICE/UDP behaviour.
type WebRTCConfig struct {
	UDPPortMin uint16 `toml:"udp_port_min"`
	UDPPortMax uint16 `toml:"udp_port_max"`
}

// VideoConfig describes the desired encoded video parameters.
type VideoConfig struct {
	Codec                 string `toml:"codec"`
	FPS                   int    `toml:"fps"`
	Bitrate               int    `toml:"bitrate"`
	KeyframeIntervalFrame int    `toml:"keyframe_interval_frames"`
}

// FFmpegConfig holds the FFmpeg command-line arguments for single-camera mode.
type FFmpegConfig struct {
	Arguments []string `toml:"arguments"`
}

// CameraInputCfg describes a single camera device for stitching.
type CameraInputCfg struct {
	Device      string `toml:"device"`       // e.g. "/dev/video0"
	Label       string `toml:"label"`        // e.g. "front"
	InputFormat string `toml:"input_format"` // e.g. "mjpeg"
	Width       int    `toml:"width"`        // capture width, e.g. 640
	Height      int    `toml:"height"`       // capture height, e.g. 480
}

// StitchCfg holds the multi-camera stitching configuration.
type StitchCfg struct {
	Enabled      bool              `toml:"enabled"`
	Cameras      []CameraInputCfg  `toml:"cameras"`
	CanvasWidth  int               `toml:"canvas_width"`  // e.g. 1920
	CanvasHeight int               `toml:"canvas_height"` // e.g. 1080
	Encoder      string            `toml:"encoder"`       // "h264_v4l2m2m" or "libx264"
	Bitrate      string            `toml:"bitrate"`       // e.g. "2500k"
}

// LoadConfig reads and parses a TOML config file.
func LoadConfig(path string) (Config, error) {
	var config Config
	fileData, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("Error reading config file: %v", err)
	}

	err = toml.Unmarshal(fileData, &config)
	if err != nil {
		log.Fatalf("Error unmarshalling TOML: %v", err)
	}
	return config, err
}
