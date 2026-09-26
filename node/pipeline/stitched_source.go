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
	Cameras         []CameraInput
	CanvasWidth     int    // output width, e.g. 1920
	CanvasHeight    int    // output height, e.g. 1080
	Encoder         string // "h264_v4l2m2m" or "libx264"
	FPS             int
	Bitrate         string // e.g. "2500k"
	ThreadQueueSize int    // V4L2 read buffer depth per camera (default 512)
	Preset          string // libx264 only: "ultrafast", "superfast", etc.
	Tune            string // libx264 only: "zerolatency", "film", etc.
	Profile         string // libx264 only: "baseline", "main", "high"
	MaxRate         string // libx264 only: VBV max rate, e.g. "2000k"
	BufSize         string // libx264 only: VBV buffer size, e.g. "150k"
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
//
// When CanvasWidth or CanvasHeight is 0, the canvas is auto-computed
// from camera count and dimensions to maximise per-camera resolution
// while avoiding unnecessary upscaling.
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
	if config.ThreadQueueSize <= 0 {
		config.ThreadQueueSize = 512
	}

	// Auto-compute canvas size when not explicitly configured.
	// This maximises per-camera resolution for fewer cameras (less
	// CPU/USB load) and avoids wasteful upscaling.
	if config.CanvasWidth <= 0 || config.CanvasHeight <= 0 {
		cols, rows := gridLayout(len(config.Cameras))
		config.CanvasWidth, config.CanvasHeight = optimalCanvas(config.Cameras, cols, rows)
		log.Printf("pipeline: auto-computed canvas %dx%d for %d cameras (%dx%d grid)",
			config.CanvasWidth, config.CanvasHeight, len(config.Cameras), cols, rows)
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

	log.Printf("pipeline: stitched FFmpeg started (pid=%d) cameras=%d encoder=%s canvas=%dx%d",
		s.cmd.Process.Pid, len(s.config.Cameras), s.config.Encoder,
		s.config.CanvasWidth, s.config.CanvasHeight)

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

// optimalCanvas computes output canvas dimensions that maximise per-camera
// resolution while avoiding unnecessary upscaling. The principle is:
// fewer cameras → each camera gets its full native resolution as the cell
// size, so the canvas is just cols×cam_w by rows×cam_h with zero scaling.
//
// Layout and resulting canvas (for 640×480 cameras):
//
//	1 camera:    1×1 → 640×480    (native, no scaling)
//	2 cameras:   2×1 → 1280×480   (native, no scaling)
//	3-4 cameras: 2×2 → 1280×960   (native, no scaling)
//	5-6 cameras: 3×2 → 1920×1080  (minimal 60px vertical pad per cell)
func optimalCanvas(cameras []CameraInput, cols, rows int) (canvasW, canvasH int) {
	// Find the maximum camera dimensions across all inputs.
	maxW, maxH := 0, 0
	for _, cam := range cameras {
		if cam.Width > maxW {
			maxW = cam.Width
		}
		if cam.Height > maxH {
			maxH = cam.Height
		}
	}

	// Base canvas: exact multiples of camera dimensions → zero scaling.
	canvasW = cols * maxW
	canvasH = rows * maxH

	// For 3×2 grids (5-6 cameras), use a 1080p canvas.
	// Width 3×640 = 1920 is already correct; height 2×480 = 960 is
	// rounded up to 1080 for a standard output resolution. Each cell
	// becomes 640×540 with just 60px of black padding vertically.
	if cols >= 3 && rows >= 2 {
		canvasW = 1920
		canvasH = 1080
	}

	return canvasW, canvasH
}

// buildFFmpegArgs constructs the complete FFmpeg argument list for
// stitching all cameras into an automatically-tiled grid on the canvas.
//
// Cell sizes are derived from the (possibly auto-computed) canvas
// dimensions divided by the grid layout, so with auto-canvas the cells
// match or closely match the camera capture resolution — no upscaling.
func (s *StitchedSource) buildFFmpegArgs() []string {
	var args []string

	numCameras := len(s.config.Cameras)
	cols, rows := gridLayout(numCameras)
	cellW := s.config.CanvasWidth / cols
	cellH := s.config.CanvasHeight / rows

	fps := strconv.Itoa(s.config.FPS)

	queueSize := strconv.Itoa(s.config.ThreadQueueSize)

	// Over-allocate threads (8) for the filter graph (decode + scale + xstack) 
	// to maximize resource usage since the Pi is dedicated to this task.
	// Crucially, use -copyts so FFmpeg does not independently reset each camera's
	// start time to 0. This preserves the absolute V4L2 hardware timestamps,
	// allowing xstack to perfectly synchronize them.
	args = append(args, "-threads", "8", "-filter_threads", "8", "-copyts")

	// Add inputs for each camera
	for _, cam := range s.config.Cameras {
		w := strconv.Itoa(cam.Width)
		h := strconv.Itoa(cam.Height)
		args = append(args,
			"-probesize", "32", // Don't buffer seconds of data before starting
			"-analyzeduration", "0", // Start immediately
			"-thread_queue_size", queueSize, // Decouple V4L2 reading from the xstack filter processing
			"-f", "v4l2",
			"-input_format", cam.InputFormat,
			"-framerate", fps,
			"-video_size", w+"x"+h,
			"-fflags", "+discardcorrupt", // Silently discard corrupt MJPEG frames instead of stalling
			"-flags", "low_delay",
			"-err_detect", "ignore_err", // Tolerate MJPEG decode errors without blocking
			"-i", cam.Device,
		)
	}

	// Build the filter_complex string
	filter := s.buildFilterComplex(numCameras, cols, rows, cellW, cellH)
	args = append(args, "-filter_complex", filter)

	// Encoder settings
	if s.config.Encoder == "libx264" || s.config.Encoder == "h264" {
		preset := s.config.Preset
		if preset == "" {
			preset = "ultrafast"
		}
		tune := s.config.Tune
		if tune == "" {
			tune = "zerolatency"
		}
		profile := s.config.Profile
		if profile == "" {
			profile = "baseline"
		}
		maxRate := s.config.MaxRate
		if maxRate == "" {
			maxRate = s.config.Bitrate
		}
		bufSize := s.config.BufSize
		if bufSize == "" {
			bufSize = "150k"
		}
		args = append(args,
			"-c:v", s.config.Encoder,
			"-preset", preset,
			"-tune", tune,
			"-profile:v", profile,
			"-level", "4.1",
			"-pix_fmt", "yuv420p",
			"-x264-params", "repeat-headers=1:keyint="+fps+":min-keyint="+fps+":aud=1:slices="+strconv.Itoa(rows)+":deblock=-1,-1",
			"-b:v", s.config.Bitrate,
			"-maxrate", maxRate,
			"-bufsize", bufSize,
		)
	} else {
		// Hardware encoders (h264_v4l2m2m, etc.)
		// These ignore preset/tune/profile — only bitrate and GOP size matter.
		args = append(args,
			"-c:v", s.config.Encoder,
			"-b:v", s.config.Bitrate,
			"-g", fps,
			"-pix_fmt", "yuv420p",
		)
	}

	// Use dump_extra to repeat SPS/PPS headers at every keyframe for WebRTC mid-stream joins.
	// Use h264_metadata=aud=insert to ensure EVERY frame has an Access Unit Delimiter (AUD).
	// Without AUDs, ReadAccessUnits fails to detect frame boundaries from hardware encoders
	// and incorrectly groups 30 frames into a single giant 1-fps chunk!
	args = append(args, "-bsf:v", "dump_extra=freq=keyframe,h264_metadata=aud=insert")

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
	// We pass the raw V4L2 timestamps directly to xstack. Because V4L2 uses the 
	// system's CLOCK_MONOTONIC, all cameras naturally share the same absolute timeline.
	// This ensures xstack stitches them in perfect real-time sync (no leading/lagging),
	// automatically waiting for or dropping frames if a camera falls behind.
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
