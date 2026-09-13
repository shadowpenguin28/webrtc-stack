package pipeline

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"time"

	"github.com/pion/webrtc/v4/pkg/media/h264reader"
)

// TestPatternSource generates H.264 video using FFmpeg.
// It implements VideoSource.
type TestPatternSource struct {
	args   []string
	fps    int
	frames chan AccessUnit
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

// NewTestPatternSource creates a video source that runs FFmpeg with the
// given arguments. ffmpegArgs should produce Annex-B H.264 on stdout.
// fps is the expected framerate, used to compute per-frame duration.
func NewTestPatternSource(ffmpegArgs []string, fps int) *TestPatternSource {
	if fps <= 0 {
		fps = 24
	}
	return &TestPatternSource{
		args:   ffmpegArgs,
		fps:    fps,
		frames: make(chan AccessUnit, 4),
	}
}

// Frames returns the channel of complete access units.
func (t *TestPatternSource) Frames() <-chan AccessUnit {
	return t.frames
}

// Start launches FFmpeg and reads Annex-B H.264 access units from stdout.
// It blocks until the context is cancelled, FFmpeg exits, or an error occurs.
func (t *TestPatternSource) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	t.cancel = cancel

	t.cmd = exec.CommandContext(ctx, "ffmpeg", t.args...)

	stdout, err := t.cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}

	// Capture stderr for diagnostics
	stderr, err := t.cmd.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("ffmpeg stderr pipe: %w", err)
	}

	if err := t.cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("ffmpeg start: %w", err)
	}

	log.Printf("pipeline: FFmpeg started (pid=%d) args=%v", t.cmd.Process.Pid, t.args)

	// Log stderr in a separate goroutine
	go t.logStderr(stderr)

	// Read access units from stdout using Pion's h264reader
	t.readAccessUnits(ctx, stdout)

	// Wait for FFmpeg to exit
	err = t.cmd.Wait()
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
func (t *TestPatternSource) Stop() {
	if t.cancel != nil {
		t.cancel()
	}
}

// readAccessUnits uses Pion's h264reader to extract NAL units from the
// Annex-B stream, groups them into access units (SPS+PPS+VCL = one AU),
// and sends complete frames to the frames channel.
//
// An access unit boundary is detected when a new VCL NAL (types 1–5)
// is encountered and we already have a VCL NAL accumulated. Non-VCL
// NALs (SPS, PPS, SEI) are grouped WITH the following VCL NAL so the
// browser receives them together in one WriteSample call.
func (t *TestPatternSource) readAccessUnits(ctx context.Context, reader io.Reader) {
	h264, err := h264reader.NewReader(reader)
	if err != nil {
		log.Printf("pipeline: failed to create h264reader: %v", err)
		return
	}

	var currentAU []byte
	var hasVCL bool
	var frameCount uint64

	annexBPrefix := []byte{0x00, 0x00, 0x00, 0x01}

	for {
		if ctx.Err() != nil {
			return
		}

		nal, err := h264.NextNAL()
		if err != nil {
			if err != io.EOF {
				log.Printf("pipeline: h264reader error: %v", err)
			}
			break
		}

		if len(nal.Data) == 0 {
			continue
		}

		nalType := nal.Data[0] & 0x1F
		isVCL := nalType >= 1 && nalType <= 5
		isAUD := nalType == 9
		isSPS := nalType == 7

		// A new Access Unit starts when we see an AUD or SPS (if we already have VCL data).
		// This correctly groups multi-slice frames (multiple VCL NALs) into a single AU.
		if (isAUD || isSPS) && hasVCL {
			t.emitAU(currentAU)
			frameCount++
			if frameCount%30 == 0 {
				log.Printf("pipeline: emitted %d frames", frameCount)
			}
			currentAU = nil
			hasVCL = false
		}

		currentAU = append(currentAU, annexBPrefix...)
		currentAU = append(currentAU, nal.Data...)

		if isVCL {
			hasVCL = true
		}
	}

	// Emit any remaining data
	if len(currentAU) > 0 {
		t.emitAU(currentAU)
	}
}

// emitAU sends a complete access unit to the frames channel, dropping
// the oldest frame if the channel is full.
func (t *TestPatternSource) emitAU(data []byte) {
	au := AccessUnit{
		Data:      data,
		Timestamp: time.Now(),
	}
	select {
	case t.frames <- au:
	default:
		// Drop oldest, keep latest
		select {
		case <-t.frames:
		default:
		}
		t.frames <- au
	}
}

// logStderr reads FFmpeg's stderr and logs the last few lines.
func (t *TestPatternSource) logStderr(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		log.Printf("pipeline: ffmpeg: %s", scanner.Text())
	}
}
