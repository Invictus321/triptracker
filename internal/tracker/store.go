// Package tracker stores the journey history and serves it over HTTP.
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
	"slices"
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

// State is the snapshot served to map viewers. Journeys are ordered oldest first; the last
// one is the current journey.
type State struct {
	Journeys   []Journey `json:"journeys"`
	ServerTime time.Time `json:"serverTime"`
}

// history is the on-disk format.
type history struct {
	Journeys []*Journey `json:"journeys"`
}

// ErrInvalidPoint is returned for out-of-range coordinates.
var ErrInvalidPoint = errors.New("invalid coordinates")

// maxFutureSkew bounds how far ahead of the server clock a device timestamp may be.
const maxFutureSkew = 5 * time.Minute

// Store holds every journey, persists them to disk and notifies subscribers of changes.
type Store struct {
	mu       sync.Mutex
	path     string
	journeys []*Journey
	subs     map[chan struct{}]struct{}
	now      func() time.Time
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
	var h history
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if h.Journeys == nil {
		// Files written before history was kept hold a single journey object.
		var j Journey
		if err := json.Unmarshal(data, &j); err == nil && j.ID != "" {
			h.Journeys = []*Journey{&j}
		}
	}
	s.journeys = h.Journeys
	return s, nil
}

// Snapshot returns a deep copy of the current state.
func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{Journeys: make([]Journey, len(s.journeys)), ServerTime: s.now().UTC()}
	for i, j := range s.journeys {
		st.Journeys[i] = *j
		st.Journeys[i].Points = slices.Clone(j.Points)
	}
	return st
}

// Start begins a new journey. It is idempotent for a journey ID that already exists.
func (s *Store) Start(journeyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if journeyID != "" && s.findLocked(journeyID) != nil {
		return nil
	}
	s.newJourneyLocked(journeyID)
	return s.commitLocked()
}

// AddPoint records a location. A known journeyID appends to that journey (late retries
// never reopen an ended journey); an unknown one starts a new journey. An empty journeyID
// appends to the active journey, or starts one if the last journey has ended.
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

	var j *Journey
	if journeyID != "" {
		j = s.findLocked(journeyID)
	} else if cur := s.currentLocked(); cur != nil && !cur.Ended {
		j = cur
	}
	if j == nil {
		j = s.newJourneyLocked(journeyID)
	}
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
	j := s.currentLocked()
	if journeyID != "" {
		j = s.findLocked(journeyID)
	}
	if j == nil {
		return false, nil
	}
	if j.Ended {
		return true, nil
	}
	now := s.now().UTC()
	j.Ended = true
	j.EndedAt = &now
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

func (s *Store) currentLocked() *Journey {
	if len(s.journeys) == 0 {
		return nil
	}
	return s.journeys[len(s.journeys)-1]
}

func (s *Store) findLocked(id string) *Journey {
	for i := len(s.journeys) - 1; i >= 0; i-- {
		if s.journeys[i].ID == id {
			return s.journeys[i]
		}
	}
	return nil
}

func (s *Store) newJourneyLocked(id string) *Journey {
	if id == "" {
		id = randomID()
	}
	j := &Journey{ID: id, StartedAt: s.now().UTC(), Points: []Point{}}
	s.journeys = append(s.journeys, j)
	return j
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
	data, err := json.Marshal(history{Journeys: s.journeys})
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
