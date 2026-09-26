package pipeline

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strconv"
)

// CameraConfig holds the parameters needed to build ffmpeg arguments
// for a single-camera source. Built from the TOML [ffmpeg] section.
type CameraConfig struct {
	Device          string // V4L2 device, e.g. "/dev/video0"
	InputFormat     string // "mjpeg" or "yuyv422"
	CaptureWidth    int    // e.g. 1920
	CaptureHeight   int    // e.g. 1080
	Encoder         string // "libx264", "h264_v4l2m2m", "h264"
	FPS             int
	Bitrate         string // e.g. "1500k"
	Preset          string // libx264 only: "ultrafast", etc.
	Tune            string // libx264 only: "zerolatency", etc.
	Profile         string // libx264 only: "baseline", etc.
	MaxRate         string // libx264 only: VBV max rate
	BufSize         string // libx264 only: VBV buffer size
	ThreadQueueSize int    // V4L2 read buffer depth (default 512)
}

// CameraSource generates H.264 video by running FFmpeg with the given
// arguments (typically capturing from a v4l2 camera device).
// It implements VideoSource.
type CameraSource struct {
	config CameraConfig
	label  string
	frames chan AccessUnit
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

// NewCameraSource creates a video source from a CameraConfig.
// fps is the expected framerate, used to compute per-frame duration.
func NewCameraSource(config CameraConfig, label string) *CameraSource {
	if config.FPS <= 0 {
		config.FPS = 30
	}
	if config.ThreadQueueSize <= 0 {
		config.ThreadQueueSize = 512
	}
	if config.Encoder == "" {
		config.Encoder = "libx264"
	}
	if config.Bitrate == "" {
		config.Bitrate = "1500k"
	}
	return &CameraSource{
		config: config,
		label:  label,
		frames: make(chan AccessUnit, 4),
	}
}

// Frames returns the channel of complete access units.
func (c *CameraSource) Frames() <-chan AccessUnit {
	return c.frames
}

// buildArgs constructs the ffmpeg argument list from config fields.
func (c *CameraSource) buildArgs() []string {
	fps := strconv.Itoa(c.config.FPS)
	w := strconv.Itoa(c.config.CaptureWidth)
	h := strconv.Itoa(c.config.CaptureHeight)
	queueSize := strconv.Itoa(c.config.ThreadQueueSize)

	var args []string

	// Input arguments
	args = append(args,
		"-thread_queue_size", queueSize,
		"-f", "v4l2",
		"-input_format", c.config.InputFormat,
		"-framerate", fps,
		"-video_size", w+"x"+h,
		"-fflags", "+discardcorrupt", // Silently discard corrupt MJPEG frames
		"-flags", "low_delay",
		"-err_detect", "ignore_err", // Tolerate decode errors without blocking
		"-i", c.config.Device,
	)

	// Encoder settings
	if c.config.Encoder == "libx264" || c.config.Encoder == "h264" {
		preset := c.config.Preset
		if preset == "" {
			preset = "ultrafast"
		}
		tune := c.config.Tune
		if tune == "" {
			tune = "zerolatency"
		}
		profile := c.config.Profile
		if profile == "" {
			profile = "baseline"
		}
		maxRate := c.config.MaxRate
		if maxRate == "" {
			maxRate = c.config.Bitrate
		}
		bufSize := c.config.BufSize
		if bufSize == "" {
			bufSize = "2000k"
		}
		args = append(args,
			"-c:v", c.config.Encoder,
			"-preset", preset,
			"-tune", tune,
			"-profile:v", profile,
			"-level", "4.1",
			"-pix_fmt", "yuv420p",
			"-x264-params", "repeat-headers=1:keyint="+fps+":min-keyint="+fps+":aud=1",
			"-b:v", c.config.Bitrate,
			"-maxrate", maxRate,
			"-bufsize", bufSize,
		)
	} else {
		// Hardware encoders (h264_v4l2m2m, etc.)
		// These ignore preset/tune/profile — only bitrate and GOP size matter.
		args = append(args,
			"-c:v", c.config.Encoder,
			"-b:v", c.config.Bitrate,
			"-g", fps,
			"-pix_fmt", "yuv420p",
		)
	}

	// Repeat SPS/PPS at keyframes and insert AUDs for WebRTC frame boundary detection
	args = append(args, "-bsf:v", "dump_extra=freq=keyframe,h264_metadata=aud=insert")

	args = append(args, "-f", "h264", "pipe:1")

	return args
}

// Start launches FFmpeg and reads Annex-B H.264 access units from stdout.
// It blocks until the context is cancelled, FFmpeg exits, or an error occurs.
func (c *CameraSource) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	args := c.buildArgs()
	c.cmd = exec.CommandContext(ctx, "ffmpeg", args...)

	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}

	// Capture stderr for diagnostics
	stderr, err := c.cmd.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("ffmpeg stderr pipe: %w", err)
	}

	if err := c.cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("ffmpeg start: %w", err)
	}

	log.Printf("pipeline: FFmpeg started (pid=%d) encoder=%s device=%s",
		c.cmd.Process.Pid, c.config.Encoder, c.config.Device)

	// Log stderr in a separate goroutine
	prefix := fmt.Sprintf("pipeline [%s]: ffmpeg", c.label)
	go LogStderr(stderr, prefix)

	// Read access units from stdout using shared helper
	ReadAccessUnits(ctx, stdout, c.frames)

	// Wait for FFmpeg to exit
	err = c.cmd.Wait()
	cancel()

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("ffmpeg exited: %w", err)
	}
	return nil
}

// Stop signals FFmpeg to terminate.
func (c *CameraSource) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
}
