// Package pipeline defines the VideoSource interface for pluggable
// video sources and the AccessUnit type representing one complete
// encoded H.264 frame, plus shared helpers for reading H.264 streams.
package pipeline

import (
	"bufio"
	"context"
	"io"
	"log"
	"time"

	"github.com/pion/webrtc/v4/pkg/media/h264reader"
)

// AccessUnit represents one complete encoded H.264 frame (one or more
// NAL units grouped as a single access unit). Never split or
// independently timestamp individual NAL units.
type AccessUnit struct {
	Data      []byte
	Timestamp time.Time
}

// VideoSource is the extension point for all video sources. Implement
// this interface to add a new video source (test pattern, FFmpeg
// capture, RealSense, etc.).
type VideoSource interface {
	// Start begins producing video frames. It blocks until the context
	// is cancelled or an unrecoverable error occurs.
	Start(ctx context.Context) error

	// Frames returns a channel of complete access units. The channel is
	// created before Start is called and remains valid for the lifetime
	// of the source.
	Frames() <-chan AccessUnit

	// Stop signals the source to shut down. It is safe to call multiple
	// times.
	Stop()
}

// ReadAccessUnits reads NAL units from an Annex-B H.264 stream, groups
// them into access units, and sends complete frames to frameCh. It
// blocks until the context is cancelled or the reader returns EOF/error.
//
// An access unit boundary is detected when a new VCL NAL (types 1–5)
// is encountered and we already have a VCL NAL accumulated. Non-VCL
// NALs (SPS, PPS, SEI) are grouped WITH the following VCL NAL so the
// decoder receives them together in one WriteSample call.
func ReadAccessUnits(ctx context.Context, reader io.Reader, frameCh chan AccessUnit) {
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
		isAUD := nalType == 9
		isSPS := nalType == 7
		isVCL := nalType >= 1 && nalType <= 5

		// A new Access Unit starts when we see an AUD or SPS (if we already have VCL data).
		// This correctly groups multi-slice frames (multiple VCL NALs) into a single AU.
		if (isAUD || isSPS) && hasVCL {
			EmitAU(frameCh, currentAU)
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
		EmitAU(frameCh, currentAU)
	}
}

// EmitAU sends a complete access unit to the frames channel, dropping
// the oldest frame if the channel is full (backpressure with drop-oldest).
func EmitAU(ch chan AccessUnit, data []byte) {
	au := AccessUnit{
		Data:      data,
		Timestamp: time.Now(),
	}
	select {
	case ch <- au:
	default:
		// Drop oldest, keep latest
		select {
		case <-ch:
		default:
		}
		ch <- au
	}
}

// LogStderr reads from an io.Reader line by line and logs each line
// with the given prefix.
func LogStderr(stderr io.Reader, prefix string) {
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		log.Printf("%s: %s", prefix, scanner.Text())
	}
}
