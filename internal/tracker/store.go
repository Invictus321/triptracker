// Package tracker stores the current journey and serves it over HTTP.
package tracker

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Point is a single GPS fix reported by the phone.
type Point struct {
	Lat      float64   `json:"lat"`
	Lon      float64   `json:"lon"`
	Accuracy float64   `json:"accuracy,omitempty"` // metres
	Battery  *int      `json:"battery,omitempty"`  // percent
	Time     time.Time `json:"time"`               // when the fix was taken (device clock)
	Received time.Time `json:"received"`           // when the server received it
}

// Journey is a sequence of points between a start and a stop signal.
type Journey struct {
	ID        string     `json:"id"`
	StartedAt time.Time  `json:"startedAt"`
	LastSeen  *time.Time `json:"lastSeen,omitempty"` // last time the phone reported a location
	Ended     bool       `json:"ended"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	Points    []Point    `json:"points"`
}

// State is the snapshot served to map viewers.
type State struct {
	Journey    *Journey  `json:"journey"`
	ServerTime time.Time `json:"serverTime"`
}

// ErrInvalidPoint is returned for out-of-range coordinates.
var ErrInvalidPoint = errors.New("invalid coordinates")

// maxFutureSkew bounds how far ahead of the server clock a device timestamp may be.
const maxFutureSkew = 5 * time.Minute

// Store holds the current journey, persists it to disk and notifies subscribers of changes.
type Store struct {
	mu      sync.Mutex
	path    string
	journey *Journey
	subs    map[chan struct{}]struct{}
	now     func() time.Time
}

// Open loads the store from path (if it exists). An empty path disables persistence.
func Open(path string) (*Store, error) {
	s := &Store{path: path, subs: map[chan struct{}]struct{}{}, now: time.Now}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.journey); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

// Snapshot returns a deep copy of the current state.
func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{ServerTime: s.now().UTC()}
	if s.journey != nil {
		j := *s.journey
		j.Points = append([]Point(nil), s.journey.Points...)
		st.Journey = &j
	}
	return st
}

// Start begins a new journey. It is idempotent for the current journey ID.
func (s *Store) Start(journeyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if journeyID != "" && s.journey != nil && s.journey.ID == journeyID {
		return nil
	}
	s.newJourneyLocked(journeyID)
	return s.commitLocked()
}

// AddPoint records a location. A journeyID different from the current one starts a
// new journey; an empty journeyID appends to the active journey (or starts one if the
// last journey has ended).
func (s *Store) AddPoint(journeyID string, p Point) error {
	if math.IsNaN(p.Lat) || math.IsNaN(p.Lon) || p.Lat < -90 || p.Lat > 90 || p.Lon < -180 || p.Lon > 180 {
		return ErrInvalidPoint
	}
	if math.IsNaN(p.Accuracy) || math.IsInf(p.Accuracy, 0) || p.Accuracy < 0 {
		p.Accuracy = 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	p.Received = now
	if p.Time.IsZero() || p.Time.After(now.Add(maxFutureSkew)) {
		p.Time = now
	}
	p.Time = p.Time.UTC()

	if s.journey == nil ||
		(journeyID != "" && s.journey.ID != journeyID) ||
		(journeyID == "" && s.journey.Ended) {
		s.newJourneyLocked(journeyID)
	}
	j := s.journey
	j.LastSeen = &now

	// Retries may resend a point the server already has; only refresh LastSeen.
	for _, existing := range j.Points {
		if existing.Time.Equal(p.Time) && existing.Lat == p.Lat && existing.Lon == p.Lon {
			return s.commitLocked()
		}
	}
	// Keep points ordered by fix time; queued points may arrive late.
	i := sort.Search(len(j.Points), func(i int) bool { return j.Points[i].Time.After(p.Time) })
	j.Points = append(j.Points, Point{})
	copy(j.Points[i+1:], j.Points[i:])
	j.Points[i] = p
	return s.commitLocked()
}

// Stop marks the journey as deliberately ended. It reports whether a journey was ended.
func (s *Store) Stop(journeyID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.journey == nil || (journeyID != "" && s.journey.ID != journeyID) {
		return false, nil
	}
	if s.journey.Ended {
		return true, nil
	}
	now := s.now().UTC()
	s.journey.Ended = true
	s.journey.EndedAt = &now
	return true, s.commitLocked()
}

// Subscribe returns a channel that receives a value whenever the state changes.
func (s *Store) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

func (s *Store) newJourneyLocked(id string) {
	if id == "" {
		id = randomID()
	}
	s.journey = &Journey{ID: id, StartedAt: s.now().UTC(), Points: []Point{}}
}

func (s *Store) commitLocked() error {
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.journey, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".journey-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
