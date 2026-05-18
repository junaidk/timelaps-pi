package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Server struct {
	cfg        *Config
	store      *SessionStore
	controller *Controller
	tmpl       *template.Template
}

func NewServer(cfg *Config, store *SessionStore, controller *Controller, templatesFS fs.FS) (*Server, error) {
	funcs := template.FuncMap{
		"fmtTime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Local().Format("2006-01-02 15:04:05")
		},
		"yesno": func(b bool) string {
			if b {
				return "yes"
			}
			return "no"
		},
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:        cfg,
		store:      store,
		controller: controller,
		tmpl:       tmpl,
	}, nil
}

func (s *Server) Routes(staticFS fs.FS) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("POST /sessions", s.handleCreateSession)
	mux.HandleFunc("GET /sessions/{id}", s.handleSession)
	mux.HandleFunc("POST /sessions/{id}/settings", s.handleUpdateSettings)
	mux.HandleFunc("POST /sessions/{id}/start", s.handleStart)
	mux.HandleFunc("POST /sessions/{id}/stop", s.handleStop)
	mux.HandleFunc("POST /sessions/{id}/compile", s.handleCompile)
	mux.HandleFunc("POST /sessions/{id}/delete", s.handleDelete)
	mux.HandleFunc("GET /sessions/{id}/status.json", s.handleStatus)
	mux.HandleFunc("GET /sessions/{id}/latest.jpg", s.handleLatestFrame)
	mux.HandleFunc("GET /sessions/{id}/videos/{file}", s.handleVideo)
	mux.HandleFunc("GET /settings", s.handleSettings)
	mux.HandleFunc("POST /settings", s.handleSaveSettings)
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	return mux
}

type flash struct {
	Kind string // "info" or "error"
	Msg  string
}

func setFlash(w http.ResponseWriter, kind, msg string) {
	v := url.QueryEscape(kind + "|" + msg)
	http.SetCookie(w, &http.Cookie{
		Name:     "flash",
		Value:    v,
		Path:     "/",
		MaxAge:   30,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func popFlash(w http.ResponseWriter, r *http.Request) *flash {
	c, err := r.Cookie("flash")
	if err != nil {
		return nil
	}
	http.SetCookie(w, &http.Cookie{
		Name:   "flash",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	raw, err := url.QueryUnescape(c.Value)
	if err != nil {
		return nil
	}
	parts := strings.SplitN(raw, "|", 2)
	if len(parts) != 2 {
		return nil
	}
	return &flash{Kind: parts[0], Msg: parts[1]}
}

type indexData struct {
	Flash         *flash
	Sessions      []*Session
	ActiveID      string
	Capturing     bool
	FramesThisRun int
	Defaults      Settings
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template error (%s): %v", name, err)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	activeID, capturing := s.controller.ActiveSession()
	st := s.controller.Status()
	data := indexData{
		Flash:         popFlash(w, r),
		Sessions:      s.store.List(),
		ActiveID:      activeID,
		Capturing:     capturing,
		FramesThisRun: st.FramesThisRun,
		Defaults:      DefaultSettings(),
	}
	s.render(w, "index.html", data)
}

func parseSettingsForm(r *http.Request) (Settings, error) {
	getInt := func(k string, def int) (int, error) {
		v := strings.TrimSpace(r.FormValue(k))
		if v == "" {
			return def, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("invalid %s: %v", k, err)
		}
		return n, nil
	}
	d := DefaultSettings()
	var s Settings
	var err error
	if s.IntervalSec, err = getInt("interval_sec", d.IntervalSec); err != nil {
		return s, err
	}
	if s.Width, err = getInt("width", d.Width); err != nil {
		return s, err
	}
	if s.Height, err = getInt("height", d.Height); err != nil {
		return s, err
	}
	if s.Quality, err = getInt("quality", d.Quality); err != nil {
		return s, err
	}
	if s.FPS, err = getInt("fps", d.FPS); err != nil {
		return s, err
	}
	return s, s.Validate()
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		setFlash(w, "error", "bad form: "+err.Error())
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	settings, err := parseSettingsForm(r)
	if err != nil {
		setFlash(w, "error", err.Error())
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	sess, err := s.store.Create(name, settings)
	if err != nil {
		setFlash(w, "error", err.Error())
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	setFlash(w, "info", "Session created.")
	http.Redirect(w, r, "/sessions/"+sess.ID, http.StatusSeeOther)
}

type sessionData struct {
	Flash         *flash
	Session       *Session
	Capturing     bool
	IsActive      bool
	FramesThisRun int
	LastError     string
	Compile       CompileStatus
	Videos        []string
	HasFrames     bool
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.store.Get(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	activeID, capturing := s.controller.ActiveSession()
	st := s.controller.Status()
	videos, _ := s.store.ListVideos(id)
	data := sessionData{
		Flash:         popFlash(w, r),
		Session:       sess,
		Capturing:     capturing,
		IsActive:      capturing && activeID == id,
		FramesThisRun: st.FramesThisRun,
		LastError:     st.LastError,
		Compile:       s.controller.CompileStatus(id),
		Videos:        videos,
		HasFrames:     sess.LastFrameNumber > 0,
	}
	s.render(w, "session.html", data)
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if active, running := s.controller.ActiveSession(); running && active == id {
		setFlash(w, "error", "stop the session before editing settings")
		http.Redirect(w, r, "/sessions/"+id, http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		setFlash(w, "error", err.Error())
		http.Redirect(w, r, "/sessions/"+id, http.StatusSeeOther)
		return
	}
	settings, err := parseSettingsForm(r)
	if err != nil {
		setFlash(w, "error", err.Error())
		http.Redirect(w, r, "/sessions/"+id, http.StatusSeeOther)
		return
	}
	if err := s.store.UpdateSettings(id, settings); err != nil {
		setFlash(w, "error", err.Error())
		http.Redirect(w, r, "/sessions/"+id, http.StatusSeeOther)
		return
	}
	setFlash(w, "info", "Settings saved.")
	http.Redirect(w, r, "/sessions/"+id, http.StatusSeeOther)
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.controller.Start(id); err != nil {
		setFlash(w, "error", err.Error())
	} else {
		setFlash(w, "info", "Capture started.")
	}
	http.Redirect(w, r, "/sessions/"+id, http.StatusSeeOther)
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.controller.Stop(); err != nil {
		setFlash(w, "error", err.Error())
	} else {
		setFlash(w, "info", "Capture stopped.")
	}
	http.Redirect(w, r, "/sessions/"+id, http.StatusSeeOther)
}

func (s *Server) handleCompile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.controller.Compile(id); err != nil {
		setFlash(w, "error", err.Error())
	} else {
		setFlash(w, "info", "Compile started — refresh in a moment.")
	}
	http.Redirect(w, r, "/sessions/"+id, http.StatusSeeOther)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if active, running := s.controller.ActiveSession(); running && active == id {
		setFlash(w, "error", "stop the session before deleting")
		http.Redirect(w, r, "/sessions/"+id, http.StatusSeeOther)
		return
	}
	if err := s.store.Delete(id); err != nil {
		setFlash(w, "error", err.Error())
		http.Redirect(w, r, "/sessions/"+id, http.StatusSeeOther)
		return
	}
	setFlash(w, "info", "Session deleted.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.store.Get(id); err != nil {
		http.NotFound(w, r)
		return
	}
	st := s.controller.Status()
	active, running := s.controller.ActiveSession()
	resp := struct {
		Capturing       bool          `json:"capturing"`
		FramesThisRun   int           `json:"frames_this_run"`
		LastFrameNumber int           `json:"last_frame_number"`
		LastFrameAt     time.Time     `json:"last_frame_at,omitzero"`
		LastError       string        `json:"last_error,omitempty"`
		Compile         CompileStatus `json:"compile"`
	}{
		Capturing:       running && active == id,
		FramesThisRun:   st.FramesThisRun,
		LastFrameNumber: st.LastFrameNumber,
		LastFrameAt:     st.LastFrameAt,
		LastError:       st.LastError,
		Compile:         s.controller.CompileStatus(id),
	}
	// Refresh last_frame_number from disk when this is not the active session.
	if !(running && active == id) {
		if sess, err := s.store.Get(id); err == nil {
			resp.LastFrameNumber = sess.LastFrameNumber
			resp.LastFrameAt = sess.LastFrameAt
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleLatestFrame(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path, err := s.store.LatestFramePath(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, path)
}

func (s *Server) handleVideo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	file := r.PathValue("file")
	// Disallow path traversal: only base names are allowed.
	if file == "" || strings.Contains(file, "/") || strings.Contains(file, "\\") || file != filepath.Base(file) {
		http.NotFound(w, r)
		return
	}
	if !strings.HasSuffix(strings.ToLower(file), ".mp4") {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.Get(id); err != nil {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(s.store.VideosDir(id), file)
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, file))
	}
	http.ServeFile(w, r, full)
}

type settingsData struct {
	Flash  *flash
	Config struct {
		DataDir         string
		Camera          string
		HardwareEncode  bool
		HardwareBitrate BitratePreset
	}
	HardwareAvailable bool
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	dataDir, camera := s.cfg.Snapshot()
	hwEnabled, hwAvailable, bitrate := s.cfg.EncodeSettings()
	data := settingsData{Flash: popFlash(w, r), HardwareAvailable: hwAvailable}
	data.Config.DataDir = dataDir
	data.Config.Camera = camera
	data.Config.HardwareEncode = hwEnabled
	data.Config.HardwareBitrate = bitrate
	s.render(w, "settings.html", data)
}

func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	if _, running := s.controller.ActiveSession(); running {
		setFlash(w, "error", "stop the active capture before changing settings")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		setFlash(w, "error", err.Error())
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	dataDir := strings.TrimSpace(r.FormValue("data_dir"))
	camera := strings.TrimSpace(r.FormValue("camera"))
	if dataDir == "" {
		setFlash(w, "error", "data dir is required")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if camera == "" {
		setFlash(w, "error", "camera is required")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	hwEncode := r.FormValue("encoder") == "hardware" && s.cfg.HardwareAvailable()
	bitrate := ValidBitrate(r.FormValue("hardware_bitrate"))
	if err := s.cfg.Update(dataDir, camera, hwEncode, bitrate); err != nil {
		setFlash(w, "error", err.Error())
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	// Re-scan sessions from the (possibly new) data directory.
	if err := s.store.reload(); err != nil {
		setFlash(w, "error", "settings saved, but reload failed: "+err.Error())
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	setFlash(w, "info", "Settings saved.")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}
