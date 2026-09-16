package pipeline

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
)

// CameraInput describes a single camera device for stitching.
type CameraInput struct {
	Device      string // e.g. "/dev/video0"
	Label       string // e.g. "front"
	InputFormat string // e.g. "mjpeg"
	Width       int    // capture width, e.g. 640
	Height      int    // capture height, e.g. 480
}

// StitchConfig holds all parameters needed to build a stitched video source.
type StitchConfig struct {
	Cameras      []CameraInput
	CanvasWidth  int    // output width, e.g. 1920
	CanvasHeight int    // output height, e.g. 1080
	Encoder      string // "h264_v4l2m2m" or "libx264"
	FPS          int
	Bitrate      string // e.g. "2500k"
}

// StitchedSource composites multiple camera feeds into a single 1080p
// H.264 stream using one FFmpeg process with an xstack filter.
// It implements VideoSource.
type StitchedSource struct {
	config StitchConfig
	frames chan AccessUnit
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

// NewStitchedSource creates a stitched video source that composites
// all configured cameras into a single canvas.
func NewStitchedSource(config StitchConfig) *StitchedSource {
	if config.FPS <= 0 {
		config.FPS = 30
	}
	if config.Bitrate == "" {
		config.Bitrate = "2500k"
	}
	if config.Encoder == "" {
		config.Encoder = "h264_v4l2m2m"
	}
	return &StitchedSource{
		config: config,
		frames: make(chan AccessUnit, 4),
	}
}

// Frames returns the channel of complete access units.
func (s *StitchedSource) Frames() <-chan AccessUnit {
	return s.frames
}

// Start launches FFmpeg with the multi-input xstack filter and reads
// Annex-B H.264 access units from stdout. It blocks until the context
// is cancelled, FFmpeg exits, or an error occurs.
func (s *StitchedSource) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	args := s.buildFFmpegArgs()
	s.cmd = exec.CommandContext(ctx, "ffmpeg", args...)

	stdout, err := s.cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}

	stderr, err := s.cmd.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("ffmpeg stderr pipe: %w", err)
	}

	if err := s.cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("ffmpeg start: %w", err)
	}

	log.Printf("pipeline: stitched FFmpeg started (pid=%d) cameras=%d encoder=%s",
		s.cmd.Process.Pid, len(s.config.Cameras), s.config.Encoder)

	go LogStderr(stderr, "pipeline: stitch-ffmpeg")

	ReadAccessUnits(ctx, stdout, s.frames)

	err = s.cmd.Wait()
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
func (s *StitchedSource) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
}

// gridLayout returns the optimal (cols, rows) for a given camera count.
//
//	1 camera  → 1×1 (full canvas)
//	2 cameras → 2×1 (side by side)
//	3 cameras → 2×2 (one empty)
//	4 cameras → 2×2
//	5 cameras → 3×2 (one empty)
//	6 cameras → 3×2
func gridLayout(numCameras int) (cols, rows int) {
	switch {
	case numCameras <= 1:
		return 1, 1
	case numCameras <= 2:
		return 2, 1
	case numCameras <= 4:
		return 2, 2
	default: // 5–6
		return 3, 2
	}
}

// buildFFmpegArgs constructs the complete FFmpeg argument list for
// stitching all cameras into an automatically-tiled grid on the canvas.
//
// The grid adapts to camera count:
//
//	1 cam:  1×1  → each cell 1920×1080
//	2 cams: 2×1  → each cell 960×1080
//	3 cams: 2×2  → each cell 960×540 (1 empty)
//	4 cams: 2×2  → each cell 960×540
//	5 cams: 3×2  → each cell 640×540 (1 empty)
//	6 cams: 3×2  → each cell 640×540
func (s *StitchedSource) buildFFmpegArgs() []string {
	var args []string

	numCameras := len(s.config.Cameras)
	cols, rows := gridLayout(numCameras)
	cellW := s.config.CanvasWidth / cols
	cellH := s.config.CanvasHeight / rows

	fps := strconv.Itoa(s.config.FPS)

	// Add inputs for each camera
	for _, cam := range s.config.Cameras {
		w := strconv.Itoa(cam.Width)
		h := strconv.Itoa(cam.Height)
		args = append(args,
			"-f", "v4l2",
			"-input_format", cam.InputFormat,
			"-framerate", fps,
			"-video_size", w+"x"+h,
			"-fflags", "nobuffer", // Prevent internal input buffering
			"-flags", "low_delay", // Optimize for low-latency live streams
			"-i", cam.Device,
		)
	}

	// Build the filter_complex string
	filter := s.buildFilterComplex(numCameras, cols, rows, cellW, cellH)
	args = append(args, "-filter_complex", filter)

	// Encoder settings
	if s.config.Encoder == "libx264" || s.config.Encoder == "h264" {
		args = append(args,
			"-c:v", s.config.Encoder,
			"-preset", "ultrafast",
			"-tune", "zerolatency",
			"-profile:v", "baseline",
			"-level", "4.1",
			"-pix_fmt", "yuv420p",
			"-x264-params", "repeat-headers=1:keyint="+fps+":min-keyint="+fps+":aud=1:slices="+strconv.Itoa(rows)+":deblock=-1,-1",
			"-b:v", s.config.Bitrate,
			"-maxrate", s.config.Bitrate,
			"-bufsize", "150k", // Extremely strict CBR buffer (max ~18KB burst)
		)
	} else {
		// Other encoders (h264, h264_v4l2m2m, etc.)
		args = append(args,
			"-c:v", s.config.Encoder,
			"-b:v", s.config.Bitrate,
			"-g", fps,
			"-pix_fmt", "yuv420p",
		)
	}

	// Use dump_extra bitstream filter to repeat SPS/PPS headers at every keyframe.
	// This is required for WebRTC clients connecting mid-stream, especially
	// when using hardware encoders that don't support -repeat_headers natively.
	args = append(args, "-bsf:v", "dump_extra=freq=keyframe")

	args = append(args, "-f", "h264", "pipe:1")

	return args
}

// buildFilterComplex generates the -filter_complex string that scales
// each camera feed to the cell size and arranges them in an xstack grid.
// Empty slots are filled with black. The output is exactly canvasWidth×canvasHeight.
func (s *StitchedSource) buildFilterComplex(numCameras, cols, rows, cellW, cellH int) string {
	totalSlots := cols * rows
	var parts []string

	// Scale each camera to fit the cell while preserving aspect ratio,
	// then pad with black to center it within the cell.
	// E.g. 640×480 (4:3) into 960×540 (16:9) → scales to 720×540, pads to 960×540
	for i := 0; i < numCameras; i++ {
		parts = append(parts,
			fmt.Sprintf("[%d:v]scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:black[s%d]",
				i, cellW, cellH, cellW, cellH, i),
		)
	}

	// Create black fill sources for empty slots
	emptyCount := totalSlots - numCameras
	for i := 0; i < emptyCount; i++ {
		parts = append(parts,
			fmt.Sprintf("color=black:%dx%d:r=%d[empty%d]", cellW, cellH, s.config.FPS, i),
		)
	}

	// Build the xstack input labels and layout positions
	var inputLabels []string
	var layoutParts []string

	slotIdx := 0
	emptyIdx := 0
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			x := col * cellW
			y := row * cellH

			if slotIdx < numCameras {
				inputLabels = append(inputLabels, fmt.Sprintf("[s%d]", slotIdx))
			} else {
				inputLabels = append(inputLabels, fmt.Sprintf("[empty%d]", emptyIdx))
				emptyIdx++
			}
			layoutParts = append(layoutParts, fmt.Sprintf("%d_%d", x, y))
			slotIdx++
		}
	}

	// xstack composites all inputs into a grid
	xstackInputs := strings.Join(inputLabels, "")
	xstackLayout := strings.Join(layoutParts, "|")
	parts = append(parts,
		fmt.Sprintf("%sxstack=inputs=%d:layout=%s", xstackInputs, totalSlots, xstackLayout),
	)

	return strings.Join(parts, ";")
}

