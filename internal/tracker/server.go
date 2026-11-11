package tracker

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxBodyBytes = 64 << 10

// Server exposes the store over HTTP.
type Server struct {
	store  *Store
	token  string
	static fs.FS
}

// NewServer returns a handler. If token is non-empty, write endpoints require it
// via "Authorization: Bearer <token>" or a "token" query parameter.
func NewServer(store *Store, token string, static fs.FS) *Server {
	return &Server{store: store, token: token, static: static}
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(s.static))
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("POST /api/location", s.authed(s.handleLocation))
	mux.HandleFunc("POST /api/start", s.authed(s.handleStart))
	mux.HandleFunc("POST /api/stop", s.authed(s.handleStop))
	return mux
}

func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			got := r.URL.Query().Get("token")
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				got = strings.TrimPrefix(h, "Bearer ")
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
				writeError(w, http.StatusUnauthorized, "invalid or missing token")
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next(w, r)
	}
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Snapshot())
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	changes, cancel := s.store.Subscribe()
	defer cancel()
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	send := func() error {
		data, err := json.Marshal(s.store.Snapshot())
		if err != nil {
			return err
		}
		if _, err := w.Write([]byte("event: state\ndata: " + string(data) + "\n\n")); err != nil {
			return err
		}
		return rc.Flush()
	}
	if err := send(); err != nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-changes:
			if err := send(); err != nil {
				return
			}
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}

type locationRequest struct {
	JourneyID string          `json:"journeyId"`
	Lat       *float64        `json:"lat"`
	Lon       *float64        `json:"lon"`
	Accuracy  float64         `json:"accuracy"`
	Battery   *int            `json:"battery"`
	Time      json.RawMessage `json:"time"` // unix milliseconds or RFC 3339 string
}

func (s *Server) handleLocation(w http.ResponseWriter, r *http.Request) {
	req, err := decodeLocation(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Lat == nil || req.Lon == nil {
		writeError(w, http.StatusBadRequest, "lat and lon are required")
		return
	}
	t, err := parseTime(req.Time)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p := Point{Lat: *req.Lat, Lon: *req.Lon, Accuracy: req.Accuracy, Battery: req.Battery, Time: t}
	if err := s.store.AddPoint(req.JourneyID, p); err != nil {
		if errors.Is(err, ErrInvalidPoint) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		log.Printf("store location: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to store location")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type journeyRequest struct {
	JourneyID string `json:"journeyId"`
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var req journeyRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.Start(req.JourneyID); err != nil {
		log.Printf("start journey: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to start journey")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	var req journeyRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ended, err := s.store.Stop(req.JourneyID)
	if err != nil {
		log.Printf("stop journey: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to stop journey")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ended": ended})
}

// decodeLocation accepts a JSON body, or form/query parameters for easy manual testing.
func decodeLocation(r *http.Request) (locationRequest, error) {
	var req locationRequest
	if isJSON(r) {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return req, errors.New("invalid JSON body")
		}
		return req, nil
	}
	if err := r.ParseForm(); err != nil {
		return req, errors.New("invalid form body")
	}
	parse := func(key string) (*float64, error) {
		v := r.Form.Get(key)
		if v == "" {
			return nil, nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, errors.New("invalid " + key)
		}
		return &f, nil
	}
	var err error
	if req.Lat, err = parse("lat"); err != nil {
		return req, err
	}
	if req.Lon, err = parse("lon"); err != nil {
		return req, err
	}
	if acc, err := parse("accuracy"); err != nil {
		return req, err
	} else if acc != nil {
		req.Accuracy = *acc
	}
	req.JourneyID = r.Form.Get("journeyId")
	if t := r.Form.Get("time"); t != "" {
		if _, err := strconv.ParseInt(t, 10, 64); err == nil {
			req.Time = json.RawMessage(t)
		} else {
			req.Time, _ = json.Marshal(t)
		}
	}
	return req, nil
}

func decodeOptionalJSON(r *http.Request, v any) error {
	if isJSON(r) {
		if err := json.NewDecoder(r.Body).Decode(v); err != nil {
			return errors.New("invalid JSON body")
		}
		return nil
	}
	if err := r.ParseForm(); err != nil {
		return errors.New("invalid form body")
	}
	if jr, ok := v.(*journeyRequest); ok {
		jr.JourneyID = r.Form.Get("journeyId")
	}
	return nil
}

func isJSON(r *http.Request) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return mt == "application/json"
}

func parseTime(raw json.RawMessage) (time.Time, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, nil
	}
	var ms int64
	if err := json.Unmarshal(raw, &ms); err == nil {
		return time.UnixMilli(ms), nil
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		if t, err := time.Parse(time.RFC3339Nano, str); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("time must be unix milliseconds or an RFC 3339 string")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}
