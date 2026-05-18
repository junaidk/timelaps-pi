package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Settings struct {
	IntervalSec int `json:"interval_sec"`
	Width       int `json:"width"`
	Height      int `json:"height"`
	Quality     int `json:"quality"`
	FPS         int `json:"fps"`
}

func DefaultSettings() Settings {
	return Settings{
		IntervalSec: 5,
		Width:       1280,
		Height:      720,
		Quality:     5,
		FPS:         30,
	}
}

func (s Settings) Validate() error {
	if s.IntervalSec < 1 {
		return errors.New("interval must be at least 1 second")
	}
	if s.Width < 16 || s.Height < 16 {
		return errors.New("resolution too small")
	}
	if s.Width > 4096 || s.Height > 4096 {
		return errors.New("resolution too large")
	}
	if s.Quality < 1 || s.Quality > 31 {
		return errors.New("quality must be between 1 and 31")
	}
	if s.FPS < 1 || s.FPS > 120 {
		return errors.New("fps must be between 1 and 120")
	}
	return nil
}

type Session struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	CreatedAt       time.Time `json:"created_at"`
	Settings        Settings  `json:"settings"`
	LastFrameNumber int       `json:"last_frame_number"`
	LastFrameAt     time.Time `json:"last_frame_at,omitzero"`
}

type SessionStore struct {
	mu       sync.RWMutex
	cfg      *Config
	sessions map[string]*Session
	order    []string
}

func NewSessionStore(cfg *Config) (*SessionStore, error) {
	s := &SessionStore{
		cfg:      cfg,
		sessions: map[string]*Session{},
	}
	if err := s.reload(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SessionStore) sessionsDir() string {
	dataDir, _ := s.cfg.Snapshot()
	return filepath.Join(dataDir, "sessions")
}

func (s *SessionStore) reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]*Session{}
	s.order = nil

	dir := s.sessionsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "session.json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var sess Session
		if err := json.Unmarshal(data, &sess); err != nil {
			continue
		}
		if sess.ID == "" {
			sess.ID = e.Name()
		}
		// Repair the frame counter from disk in case it drifted.
		if n, err := s.scanLastFrameNumber(&sess); err == nil && n > sess.LastFrameNumber {
			sess.LastFrameNumber = n
		}
		s.sessions[sess.ID] = &sess
		s.order = append(s.order, sess.ID)
	}
	sort.SliceStable(s.order, func(i, j int) bool {
		return s.sessions[s.order[i]].CreatedAt.After(s.sessions[s.order[j]].CreatedAt)
	})
	return nil
}

func (s *SessionStore) List() []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Session, 0, len(s.order))
	for _, id := range s.order {
		cp := *s.sessions[id]
		out = append(out, &cp)
	}
	return out
}

func (s *SessionStore) Get(id string) (*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %q not found", id)
	}
	cp := *sess
	return &cp, nil
}

var nameSanitizeRe = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "session"
	}
	name = nameSanitizeRe.ReplaceAllString(name, "-")
	if len(name) > 48 {
		name = name[:48]
	}
	return name
}

func newSessionID(name string) string {
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("%s-%d-%s", sanitizeName(name), time.Now().Unix(), hex.EncodeToString(buf))
}

func (s *SessionStore) Create(name string, settings Settings) (*Session, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("name is required")
	}
	sess := &Session{
		ID:        newSessionID(name),
		Name:      name,
		CreatedAt: time.Now(),
		Settings:  settings,
	}
	if err := os.MkdirAll(s.framesDir(sess), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.videosDir(sess), 0o755); err != nil {
		return nil, err
	}
	if err := s.write(sess); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.order = append([]string{sess.ID}, s.order...)
	s.mu.Unlock()
	cp := *sess
	return &cp, nil
}

func (s *SessionStore) UpdateSettings(id string, settings Settings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	sess.Settings = settings
	return s.write(sess)
}

func (s *SessionStore) Save(sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.sessions[sess.ID]; ok {
		*existing = *sess
		return s.write(existing)
	}
	cp := *sess
	s.sessions[sess.ID] = &cp
	s.order = append([]string{sess.ID}, s.order...)
	return s.write(&cp)
}

func (s *SessionStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return fmt.Errorf("session %q not found", id)
	}
	dir := filepath.Join(s.sessionsDir(), id)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	delete(s.sessions, id)
	for i, x := range s.order {
		if x == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return nil
}

func (s *SessionStore) write(sess *Session) error {
	dir := filepath.Join(s.sessionsDir(), sess.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "session.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "session.json"))
}

func (s *SessionStore) framesDir(sess *Session) string {
	return filepath.Join(s.sessionsDir(), sess.ID, "frames")
}

func (s *SessionStore) videosDir(sess *Session) string {
	return filepath.Join(s.sessionsDir(), sess.ID, "videos")
}

func (s *SessionStore) FramesDir(id string) string {
	return filepath.Join(s.sessionsDir(), id, "frames")
}

func (s *SessionStore) VideosDir(id string) string {
	return filepath.Join(s.sessionsDir(), id, "videos")
}

var framePattern = regexp.MustCompile(`^frame_(\d{6})\.jpg$`)

// scanLastFrameNumber walks the session's frames dir and returns the highest
// frame number found, or 0 if there are no frames.
func (s *SessionStore) scanLastFrameNumber(sess *Session) (int, error) {
	dir := s.framesDir(sess)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	max := 0
	for _, e := range entries {
		m := framePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	return max, nil
}

func (s *SessionStore) ScanLastFrameNumber(id string) (int, error) {
	s.mu.RLock()
	sess, ok := s.sessions[id]
	s.mu.RUnlock()
	if !ok {
		return 0, fmt.Errorf("session %q not found", id)
	}
	return s.scanLastFrameNumber(sess)
}

// FirstFrameNumber returns the lowest frame number in the session's frames dir
// (used by the compiler to set -start_number). Returns 0 when no frames exist.
func (s *SessionStore) FirstFrameNumber(id string) (int, error) {
	s.mu.RLock()
	sess, ok := s.sessions[id]
	s.mu.RUnlock()
	if !ok {
		return 0, fmt.Errorf("session %q not found", id)
	}
	dir := s.framesDir(sess)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	min := -1
	for _, e := range entries {
		m := framePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		if min == -1 || n < min {
			min = n
		}
	}
	if min == -1 {
		return 0, nil
	}
	return min, nil
}

func (s *SessionStore) LatestFramePath(id string) (string, error) {
	n, err := s.ScanLastFrameNumber(id)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", os.ErrNotExist
	}
	return filepath.Join(s.FramesDir(id), fmt.Sprintf("frame_%06d.jpg", n)), nil
}

func (s *SessionStore) ListVideos(id string) ([]string, error) {
	dir := s.VideosDir(id)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(e.Name()), ".mp4") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}
