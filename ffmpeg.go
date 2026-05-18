package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"os"
	"os/exec"
	"strings"
	"time"
)

type Capturer interface {
	Capture(ctx context.Context, s *Session, framePath string) error
}

type RealCapturer struct {
	cfg *Config
}

func (r *RealCapturer) Capture(ctx context.Context, s *Session, framePath string) error {
	_, camera := r.cfg.Snapshot()
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "v4l2",
		"-video_size", fmt.Sprintf("%dx%d", s.Settings.Width, s.Settings.Height),
		"-i", camera,
		"-frames:v", "1",
		"-q:v", fmt.Sprintf("%d", s.Settings.Quality),
		framePath,
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg capture failed: %v: %s", err, stderr.String())
	}
	return nil
}

type FakeCapturer struct{}

func (FakeCapturer) Capture(ctx context.Context, s *Session, framePath string) error {
	w, h := s.Settings.Width, s.Settings.Height
	if w <= 0 || h <= 0 {
		w, h = 320, 240
	}
	if w > 1024 {
		w = 1024
	}
	if h > 768 {
		h = 768
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	t := time.Now()
	hue := uint8((t.Unix() * 13) % 256)
	bg := color.RGBA{R: hue, G: 255 - hue, B: uint8((int(hue) + 128) % 256), A: 255}
	draw.Draw(img, img.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)
	// Draw a simple diagonal band so something obviously changes frame to frame.
	band := color.RGBA{R: 255 - hue, G: hue, B: 255, A: 255}
	for y := 0; y < h; y++ {
		x := (int(t.UnixNano()/int64(time.Millisecond)/50) + y) % w
		for dx := 0; dx < 16 && x+dx < w; dx++ {
			img.Set(x+dx, y, band)
		}
	}
	f, err := os.Create(framePath)
	if err != nil {
		return err
	}
	defer f.Close()
	q := 90
	if s.Settings.Quality >= 1 && s.Settings.Quality <= 31 {
		// Map ffmpeg q:v 1..31 (lower=better) to JPEG 1..100 (higher=better).
		q = int(100.0 - (float64(s.Settings.Quality-1) / 30.0 * 70.0))
	}
	return jpeg.Encode(f, img, &jpeg.Options{Quality: q})
}

// compileArgs builds the ffmpeg invocation that turns the session's JPEG
// sequence into an MP4 timelapse, using the software libx264 encoder.
func compileArgs(framesDir, outputPath string, fps, startNumber int) []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-framerate", fmt.Sprintf("%d", fps),
		"-start_number", fmt.Sprintf("%d", startNumber),
		"-i", fmt.Sprintf("%s/frame_%%06d.jpg", framesDir),
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-tune", "stillimage",
		"-pix_fmt", "yuv420p",
		"-crf", "23",
		outputPath,
	}
}

// compileArgsHW builds the same pipeline using the Raspberry Pi's V4L2 M2M
// hardware H.264 encoder. The encoder ignores -preset / -crf / -tune; bitrate
// is the only quality knob, so we expose it as a low/medium/high preset.
func compileArgsHW(framesDir, outputPath string, fps, startNumber int, bitrate BitratePreset) []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-framerate", fmt.Sprintf("%d", fps),
		"-start_number", fmt.Sprintf("%d", startNumber),
		"-i", fmt.Sprintf("%s/frame_%%06d.jpg", framesDir),
		"-c:v", "h264_v4l2m2m",
		"-b:v", bitrate.FFArg(),
		"-pix_fmt", "yuv420p",
		outputPath,
	}
}

// ProbeHardwareEncoder runs `ffmpeg -encoders` once and reports whether the
// h264_v4l2m2m encoder is available. Returns false (without error) if ffmpeg
// itself is missing — the caller should treat that as "no hardware encode".
func ProbeHardwareEncoder(ctx context.Context) bool {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "ffmpeg", "-hide_banner", "-encoders")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "h264_v4l2m2m")
}
