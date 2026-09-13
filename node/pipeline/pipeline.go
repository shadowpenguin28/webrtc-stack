// Package pipeline defines the VideoSource interface for pluggable
// video sources and the AccessUnit type representing one complete
// encoded H.264 frame.
package pipeline

import (
	"context"
	"time"
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
