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

// FFmpegConfig holds the parameters for single-camera mode.
// The Go code assembles these into ffmpeg command-line arguments.
type FFmpegConfig struct {
	// Device is the V4L2 camera device path, e.g. "/dev/video0".
	Device string `toml:"device"`

	// InputFormat is the V4L2 pixel format to request from the camera.
	// Options: "mjpeg" (recommended, lower bandwidth), "yuyv422" (raw, high bandwidth).
	InputFormat string `toml:"input_format"`

	// CaptureWidth and CaptureHeight set the camera capture resolution.
	// Common values: 640x480, 1280x720, 1920x1080.
	// Lower resolutions reduce CPU/USB load significantly.
	CaptureWidth  int `toml:"capture_width"`
	CaptureHeight int `toml:"capture_height"`

	// Encoder selects the H.264 encoder.
	// Options:
	//   "libx264"       — Software encoder. Works everywhere, high CPU usage.
	//                     Supports preset/tune/profile options below.
	//   "h264_v4l2m2m"  — Hardware encoder (Raspberry Pi VideoCore).
	//                     Much lower CPU usage. Ignores preset/tune/profile.
	//   "h264"          — Alias for libx264 in most ffmpeg builds.
	Encoder string `toml:"encoder"`

	// Preset controls the libx264 encoding speed/quality tradeoff.
	// Only used when encoder is "libx264" or "h264". Ignored for hardware encoders.
	// Options (fastest to slowest): "ultrafast", "superfast", "veryfast", "faster",
	//   "fast", "medium", "slow", "slower", "veryslow".
	// Recommended: "ultrafast" for real-time streaming on low-power devices.
	Preset string `toml:"preset"`

	// Tune optimizes libx264 for specific content types.
	// Only used when encoder is "libx264" or "h264". Ignored for hardware encoders.
	// Options: "zerolatency" (recommended for live streaming — disables B-frames
	//   and reduces lookahead), "film", "animation", "grain", "stillimage".
	Tune string `toml:"tune"`

	// Profile sets the H.264 profile for decoder compatibility.
	// Only used when encoder is "libx264" or "h264". Ignored for hardware encoders.
	// Options: "baseline" (widest compatibility, no B-frames),
	//   "main" (B-frames allowed), "high" (best compression).
	// Recommended: "baseline" for WebRTC.
	Profile string `toml:"profile"`

	// Bitrate is the target video bitrate, e.g. "1500k", "2500k", "4M".
	Bitrate string `toml:"bitrate"`

	// MaxRate caps the maximum instantaneous bitrate (VBV).
	// Only used when encoder is "libx264" or "h264". Ignored for hardware encoders.
	// Should be >= bitrate. E.g. "2000k".
	MaxRate string `toml:"maxrate"`

	// BufSize is the VBV buffer size, controlling bitrate variability.
	// Only used when encoder is "libx264" or "h264". Ignored for hardware encoders.
	// Smaller values = stricter CBR. E.g. "2000k" (normal), "150k" (very strict).
	BufSize string `toml:"bufsize"`

	// ThreadQueueSize sets the number of packets to buffer from the V4L2 device.
	// Higher values decouple camera reading from encoding, preventing drops
	// when the encoder briefly stalls. Default: 512. Range: 8–4096.
	ThreadQueueSize int `toml:"thread_queue_size"`
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
	Enabled      bool             `toml:"enabled"`
	Cameras      []CameraInputCfg `toml:"cameras"`
	CanvasWidth  int              `toml:"canvas_width"`  // e.g. 1920
	CanvasHeight int              `toml:"canvas_height"` // e.g. 1080

	// Encoder selects the H.264 encoder for the stitched output.
	// Options: "h264_v4l2m2m" (hardware, recommended for Pi),
	//   "libx264" (software), "h264" (alias for libx264).
	Encoder string `toml:"encoder"`

	// Bitrate is the target bitrate for the stitched output, e.g. "1500k", "2500k".
	Bitrate string `toml:"bitrate"`

	// ThreadQueueSize sets per-camera V4L2 read buffer depth.
	// Higher values prevent frame drops when one camera stalls.
	// Default: 512. Range: 8–4096.
	ThreadQueueSize int `toml:"thread_queue_size"`

	// Preset controls the libx264 encoding speed/quality tradeoff.
	// Only used when encoder is "libx264" or "h264". Ignored for hardware encoders.
	// Options: "ultrafast", "superfast", "veryfast", "faster", "fast",
	//   "medium", "slow", "slower", "veryslow".
	Preset string `toml:"preset"`

	// Tune optimizes libx264 for specific content types.
	// Only used when encoder is "libx264" or "h264". Ignored for hardware encoders.
	// Options: "zerolatency" (recommended), "film", "animation", "grain".
	Tune string `toml:"tune"`

	// Profile sets the H.264 profile.
	// Only used when encoder is "libx264" or "h264". Ignored for hardware encoders.
	// Options: "baseline" (recommended for WebRTC), "main", "high".
	Profile string `toml:"profile"`

	// MaxRate caps the maximum instantaneous bitrate (VBV).
	// Only used when encoder is "libx264" or "h264".
	MaxRate string `toml:"maxrate"`

	// BufSize is the VBV buffer size.
	// Only used when encoder is "libx264" or "h264".
	BufSize string `toml:"bufsize"`
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
