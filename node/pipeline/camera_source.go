package pipeline

import (
	"context"
	"fmt"
	"log"
	"os/exec"
)

// CameraSource generates H.264 video by running FFmpeg with the given
// arguments (typically capturing from a v4l2 camera device).
// It implements VideoSource.
type CameraSource struct {
	args   []string
	fps    int
	frames chan AccessUnit
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

// NewCameraSource creates a video source that runs FFmpeg with the
// given arguments. ffmpegArgs should produce Annex-B H.264 on stdout.
// fps is the expected framerate, used to compute per-frame duration.
func NewCameraSource(ffmpegArgs []string, fps int) *CameraSource {
	if fps <= 0 {
		fps = 24
	}
	return &CameraSource{
		args:   ffmpegArgs,
		fps:    fps,
		frames: make(chan AccessUnit, 4),
	}
}

// Frames returns the channel of complete access units.
func (c *CameraSource) Frames() <-chan AccessUnit {
	return c.frames
}

// Start launches FFmpeg and reads Annex-B H.264 access units from stdout.
// It blocks until the context is cancelled, FFmpeg exits, or an error occurs.
func (c *CameraSource) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	c.cmd = exec.CommandContext(ctx, "ffmpeg", c.args...)

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

	log.Printf("pipeline: FFmpeg started (pid=%d) args=%v", c.cmd.Process.Pid, c.args)

	// Log stderr in a separate goroutine
	go LogStderr(stderr, "pipeline: ffmpeg")

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
