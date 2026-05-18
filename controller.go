package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type CaptureStatus struct {
	Running         bool      `json:"running"`
	SessionID       string    `json:"session_id,omitempty"`
	StartedAt       time.Time `json:"started_at,omitzero"`
	FramesThisRun   int       `json:"frames_this_run"`
	LastFrameNumber int       `json:"last_frame_number"`
	LastFrameAt     time.Time `json:"last_frame_at,omitzero"`
	LastError       string    `json:"last_error,omitempty"`
}

type CompileStatus struct {
	Running   bool      `json:"running"`
	StartedAt time.Time `json:"started_at,omitzero"`
	Output    string    `json:"output,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Warning   string    `json:"warning,omitempty"`
	Encoder   string    `json:"encoder,omitempty"`
}

type Controller struct {
	mu sync.Mutex

	cfg      *Config
	store    *SessionStore
	capturer Capturer

	// capture state
	running         bool
	sessionID       string
	startedAt       time.Time
	framesThisRun   int
	lastFrameNumber int
	lastFrameAt     time.Time
	lastErr         string
	cancel          context.CancelFunc
	done            chan struct{}

	// compile state, keyed by session id
	compileMu     sync.Mutex
	compileStates map[string]*CompileStatus
}

func NewController(cfg *Config, store *SessionStore, capturer Capturer) *Controller {
	return &Controller{
		cfg:           cfg,
		store:         store,
		capturer:      capturer,
		compileStates: map[string]*CompileStatus{},
	}
}

func (c *Controller) Status() CaptureStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CaptureStatus{
		Running:         c.running,
		SessionID:       c.sessionID,
		StartedAt:       c.startedAt,
		FramesThisRun:   c.framesThisRun,
		LastFrameNumber: c.lastFrameNumber,
		LastFrameAt:     c.lastFrameAt,
		LastError:       c.lastErr,
	}
}

func (c *Controller) ActiveSession() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID, c.running
}

func (c *Controller) Start(sessionID string) error {
	c.mu.Lock()
	if c.running {
		active := c.sessionID
		c.mu.Unlock()
		if active == sessionID {
			return errors.New("this session is already capturing")
		}
		return fmt.Errorf("another session (%s) is already capturing", active)
	}

	sess, err := c.store.Get(sessionID)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if err := sess.Settings.Validate(); err != nil {
		c.mu.Unlock()
		return err
	}

	startFrom, err := c.store.ScanLastFrameNumber(sessionID)
	if err != nil {
		c.mu.Unlock()
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.running = true
	c.sessionID = sessionID
	c.startedAt = time.Now()
	c.framesThisRun = 0
	c.lastFrameNumber = startFrom
	c.lastFrameAt = time.Time{}
	c.lastErr = ""
	c.cancel = cancel
	c.done = make(chan struct{})
	c.mu.Unlock()

	go c.run(ctx, sessionID, startFrom)
	return nil
}

func (c *Controller) run(ctx context.Context, sessionID string, startFrom int) {
	defer func() {
		c.mu.Lock()
		c.running = false
		done := c.done
		c.done = nil
		c.cancel = nil
		c.mu.Unlock()
		if done != nil {
			close(done)
		}
	}()

	sess, err := c.store.Get(sessionID)
	if err != nil {
		c.setLastErr(err.Error())
		return
	}
	interval := max(time.Duration(sess.Settings.IntervalSec)*time.Second, time.Second)

	framesDir := c.store.FramesDir(sessionID)
	if err := os.MkdirAll(framesDir, 0o755); err != nil {
		c.setLastErr(err.Error())
		return
	}

	current := startFrom
	persistEvery := 10
	lastPersist := time.Now()
	maxPersistInterval := 30 * time.Second

	// Capture one frame immediately, then on each tick.
	tick := func() {
		current++
		framePath := filepath.Join(framesDir, fmt.Sprintf("frame_%06d.jpg", current))
		err := c.captureWithRetry(ctx, sess, framePath)
		if err != nil {
			current-- // free the number; retry on next tick
			c.setLastErr(err.Error())
			log.Printf("capture error (session=%s): %v", sessionID, err)
			return
		}
		now := time.Now()
		sess.LastFrameNumber = current
		sess.LastFrameAt = now
		c.mu.Lock()
		c.framesThisRun++
		c.lastFrameNumber = current
		c.lastFrameAt = now
		c.mu.Unlock()

		if c.framesThisRun%persistEvery == 0 || time.Since(lastPersist) > maxPersistInterval {
			if err := c.store.Save(sess); err != nil {
				log.Printf("session save error: %v", err)
			}
			lastPersist = time.Now()
		}
	}

	tick()
	if ctx.Err() != nil {
		_ = c.store.Save(sess)
		return
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = c.store.Save(sess)
			return
		case <-t.C:
			tick()
		}
	}
}

func (c *Controller) Stop() error {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return errors.New("no session is currently capturing")
	}
	cancel := c.cancel
	done := c.done
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	return nil
}

// captureWithRetry handles the V4L2 quirk where the kernel briefly keeps
// /dev/video0 marked busy after the previous ffmpeg child exits. We retry
// only on that specific error (otherwise a real failure should surface
// immediately on the next tick).
func (c *Controller) captureWithRetry(ctx context.Context, sess *Session, framePath string) error {
	const maxAttempts = 4
	backoff := 200 * time.Millisecond
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		captureCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = c.capturer.Capture(captureCtx, sess, framePath)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		if attempt == maxAttempts || !isDeviceBusyErr(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return err
}

func isDeviceBusyErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "device or resource busy") ||
		strings.Contains(s, "resource busy") ||
		strings.Contains(s, "ebusy")
}

func (c *Controller) setLastErr(msg string) {
	c.mu.Lock()
	c.lastErr = msg
	c.mu.Unlock()
}

func (c *Controller) CompileStatus(sessionID string) CompileStatus {
	c.compileMu.Lock()
	defer c.compileMu.Unlock()
	if st, ok := c.compileStates[sessionID]; ok {
		return *st
	}
	return CompileStatus{}
}

func (c *Controller) AnyCompileRunning() (string, bool) {
	c.compileMu.Lock()
	defer c.compileMu.Unlock()
	for id, st := range c.compileStates {
		if st.Running {
			return id, true
		}
	}
	return "", false
}

func (c *Controller) Compile(sessionID string) error {
	if active, running := c.ActiveSession(); running && active == sessionID {
		return errors.New("cannot compile while this session is capturing")
	}
	if id, busy := c.AnyCompileRunning(); busy {
		return fmt.Errorf("another compile is already running (session %s)", id)
	}
	sess, err := c.store.Get(sessionID)
	if err != nil {
		return err
	}
	if sess.LastFrameNumber == 0 {
		// Double-check by scanning disk, in case state is out of sync.
		n, _ := c.store.ScanLastFrameNumber(sessionID)
		if n == 0 {
			return errors.New("no frames to compile")
		}
	}

	first, err := c.store.FirstFrameNumber(sessionID)
	if err != nil {
		return err
	}
	if first == 0 {
		return errors.New("no frames to compile")
	}
	framesDir := c.store.FramesDir(sessionID)
	videosDir := c.store.VideosDir(sessionID)
	if err := os.MkdirAll(videosDir, 0o755); err != nil {
		return err
	}
	outName := fmt.Sprintf("timelapse-%s.mp4", time.Now().Format("20060102-150405"))
	outPath := filepath.Join(videosDir, outName)

	c.compileMu.Lock()
	c.compileStates[sessionID] = &CompileStatus{
		Running:   true,
		StartedAt: time.Now(),
	}
	c.compileMu.Unlock()

	go c.runCompile(sessionID, framesDir, outPath, outName, sess.Settings.FPS, first)
	return nil
}

func (c *Controller) runCompile(sessionID, framesDir, outPath, outName string, fps, startNumber int) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Hour)
	defer cancel()

	hwEnabled, hwAvailable, bitrate := c.cfg.EncodeSettings()
	tryHW := hwEnabled && hwAvailable

	encoderUsed := "libx264"
	var warning string
	var runErr error
	var stderr bytes.Buffer

	if tryHW {
		encoderUsed = "h264_v4l2m2m"
		args := compileArgsHW(framesDir, outPath, fps, startNumber, bitrate)
		cmd := exec.CommandContext(ctx, "ffmpeg", args...)
		cmd.Stderr = &stderr
		runErr = cmd.Run()
		if runErr != nil && ctx.Err() == nil {
			// Hardware path failed — fall back to libx264 and keep a warning.
			warning = fmt.Sprintf("hardware encoder (h264_v4l2m2m) failed; falling back to libx264: %s", strings.TrimSpace(stderr.String()))
			log.Printf("compile (session=%s): %s", sessionID, warning)
			_ = os.Remove(outPath)
			stderr.Reset()
			encoderUsed = "libx264"
			args = compileArgs(framesDir, outPath, fps, startNumber)
			cmd = exec.CommandContext(ctx, "ffmpeg", args...)
			cmd.Stderr = &stderr
			runErr = cmd.Run()
		}
	} else {
		args := compileArgs(framesDir, outPath, fps, startNumber)
		cmd := exec.CommandContext(ctx, "ffmpeg", args...)
		cmd.Stderr = &stderr
		runErr = cmd.Run()
	}

	c.compileMu.Lock()
	st := c.compileStates[sessionID]
	st.Running = false
	st.Encoder = encoderUsed
	st.Warning = warning
	if runErr != nil {
		_ = os.Remove(outPath)
		st.LastError = fmt.Sprintf("compile failed: %v: %s", runErr, stderr.String())
		st.Output = ""
	} else {
		st.LastError = ""
		st.Output = outName
	}
	c.compileMu.Unlock()
}
