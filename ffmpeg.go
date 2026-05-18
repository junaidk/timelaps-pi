package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
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

// Streamer produces an MJPEG byte stream for the viewfinder. Implementations
// must honour ctx.Done(): a cancelled context means "release the camera now".
type Streamer interface {
	Stream(ctx context.Context, w io.Writer) error
}

// viewfinderBoundary is the multipart boundary the ffmpeg mpjpeg muxer uses,
// and the one we synthesize in the fake streamer. Must match the Content-Type
// header the handler sets.
const viewfinderBoundary = "ffmpeg"

type RealStreamer struct {
	cfg *Config
}

func (r *RealStreamer) Stream(ctx context.Context, w io.Writer) error {
	_, camera := r.cfg.Snapshot()
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "v4l2",
		"-video_size", "1280x720",
		"-i", camera,
		"-f", "mpjpeg",
		"-q:v", "5",
		"pipe:1",
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ffmpeg viewfinder failed: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

type FakeStreamer struct{}

func (FakeStreamer) Stream(ctx context.Context, w io.Writer) error {
	const fw, fh = 640, 480
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		img := image.NewRGBA(image.Rect(0, 0, fw, fh))
		t := time.Now()
		hue := uint8((t.UnixMilli() / 50) % 256)
		bg := color.RGBA{R: hue, G: 255 - hue, B: uint8((int(hue) + 128) % 256), A: 255}
		draw.Draw(img, img.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)
		band := color.RGBA{R: 255 - hue, G: hue, B: 255, A: 255}
		for y := 0; y < fh; y++ {
			x := (int(t.UnixMilli()/30) + y) % fw
			for dx := 0; dx < 16 && x+dx < fw; dx++ {
				img.Set(x+dx, y, band)
			}
		}
		var jbuf bytes.Buffer
		if err := jpeg.Encode(&jbuf, img, &jpeg.Options{Quality: 75}); err != nil {
			return err
		}
		header := fmt.Sprintf("--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", viewfinderBoundary, jbuf.Len())
		if _, err := io.WriteString(w, header); err != nil {
			return nil
		}
		if _, err := w.Write(jbuf.Bytes()); err != nil {
			return nil
		}
		if _, err := io.WriteString(w, "\r\n"); err != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(100 * time.Millisecond):
		}
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
