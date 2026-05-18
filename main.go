package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dataDir := flag.String("data-dir", "./data", "default data directory (can be changed in UI)")
	camera := flag.String("camera", "/dev/video0", "default camera device (can be changed in UI)")
	fakeCamera := flag.Bool("fake-camera", false, "generate synthetic frames instead of using ffmpeg (dev mode)")
	flag.Parse()

	absDataDir, err := filepath.Abs(*dataDir)
	if err != nil {
		log.Fatalf("resolve data dir: %v", err)
	}
	if err := os.MkdirAll(absDataDir, 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}
	cfgPath := filepath.Join(absDataDir, "config.json")
	cfg, err := LoadConfig(cfgPath, absDataDir, *camera)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	store, err := NewSessionStore(cfg)
	if err != nil {
		log.Fatalf("init session store: %v", err)
	}

	var capturer Capturer = &RealCapturer{cfg: cfg}
	if *fakeCamera {
		capturer = FakeCapturer{}
		log.Printf("using fake camera — no ffmpeg/v4l2 needed")
	}

	// Probe once at startup for h264_v4l2m2m. Result feeds the /settings UI
	// and the runCompile fallback path.
	hwAvailable := ProbeHardwareEncoder(context.Background())
	cfg.SetHardwareAvailable(hwAvailable)
	if hwAvailable {
		log.Printf("hardware encoder h264_v4l2m2m available")
	} else {
		log.Printf("hardware encoder h264_v4l2m2m not available — software encoding only")
	}

	controller := NewController(cfg, store, capturer)

	staticSub, err := fs.Sub(staticFS, ".")
	if err != nil {
		log.Fatalf("static fs: %v", err)
	}
	srv, err := NewServer(cfg, store, controller, templatesFS)
	if err != nil {
		log.Fatalf("init server: %v", err)
	}

	mux := srv.Routes(staticSub)

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("snapshoter listening on %s (data dir: %s)", *addr, absDataDir)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	log.Printf("shutting down…")

	if _, running := controller.ActiveSession(); running {
		log.Printf("stopping active capture…")
		_ = controller.Stop()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
}
