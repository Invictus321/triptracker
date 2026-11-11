package tracker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func newTestStore(t *testing.T, path string) (*Store, *time.Time) {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	return s, &clock
}

func TestAddPointOrdersAndDedupes(t *testing.T) {
	s, clock := newTestStore(t, "")
	t0 := clock.Add(-3 * time.Minute)
	for _, p := range []Point{
		{Lat: 1, Lon: 1, Time: t0},
		{Lat: 3, Lon: 3, Time: t0.Add(2 * time.Minute)},
		{Lat: 2, Lon: 2, Time: t0.Add(time.Minute)}, // delivered late from the phone's queue
	} {
		if err := s.AddPoint("j1", p); err != nil {
			t.Fatal(err)
		}
	}
	*clock = clock.Add(time.Minute)
	if err := s.AddPoint("j1", Point{Lat: 3, Lon: 3, Time: t0.Add(2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}

	j := s.Snapshot().Journey
	if len(j.Points) != 3 {
		t.Fatalf("got %d points, want 3", len(j.Points))
	}
	for i, want := range []float64{1, 2, 3} {
		if j.Points[i].Lat != want {
			t.Errorf("point %d lat = %v, want %v", i, j.Points[i].Lat, want)
		}
	}
	if !j.LastSeen.Equal(*clock) {
		t.Errorf("duplicate should refresh lastSeen: got %v, want %v", j.LastSeen, *clock)
	}
}

func TestStopAndNewJourney(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journey.json")
	s, _ := newTestStore(t, path)
	if err := s.AddPoint("j1", Point{Lat: 1, Lon: 1}); err != nil {
		t.Fatal(err)
	}
	if ended, err := s.Stop("other"); err != nil || ended {
		t.Fatalf("stop for unknown journey: ended=%v err=%v", ended, err)
	}
	if ended, err := s.Stop("j1"); err != nil || !ended {
		t.Fatalf("stop: ended=%v err=%v", ended, err)
	}

	reloaded, _ := newTestStore(t, path)
	if j := reloaded.Snapshot().Journey; j == nil || !j.Ended || len(j.Points) != 1 {
		t.Fatalf("persisted journey not restored: %+v", j)
	}

	// A late retry for the ended journey is kept but does not reopen it.
	if err := s.AddPoint("j1", Point{Lat: 2, Lon: 2}); err != nil {
		t.Fatal(err)
	}
	if j := s.Snapshot().Journey; !j.Ended || len(j.Points) != 2 {
		t.Fatalf("late point should not reopen journey: %+v", j)
	}

	if err := s.AddPoint("", Point{Lat: 5, Lon: 5}); err != nil {
		t.Fatal(err)
	}
	j := s.Snapshot().Journey
	if j.ID == "j1" || j.Ended || len(j.Points) != 1 {
		t.Fatalf("expected a fresh journey after stop, got %+v", j)
	}
}

func TestAddPointRejectsInvalidCoordinates(t *testing.T) {
	s, _ := newTestStore(t, "")
	if err := s.AddPoint("", Point{Lat: 91, Lon: 0}); err != ErrInvalidPoint {
		t.Fatalf("got %v, want ErrInvalidPoint", err)
	}
}

func TestHandlers(t *testing.T) {
	s, _ := newTestStore(t, "")
	h := NewServer(s, "secret", fstest.MapFS{"index.html": {Data: []byte("map")}}).Handler()

	do := func(method, target, auth, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		if body != "" && strings.HasPrefix(body, "{") {
			req.Header.Set("Content-Type", "application/json")
		} else if body != "" {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := do("POST", "/api/location", "", `{"lat":1,"lon":2}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: got %d", rec.Code)
	}
	if rec := do("POST", "/api/location", "secret", `{"lat":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing lon: got %d", rec.Code)
	}
	if rec := do("POST", "/api/start", "secret", `{"journeyId":"abc"}`); rec.Code != http.StatusOK {
		t.Fatalf("start: got %d %s", rec.Code, rec.Body)
	}
	if rec := do("POST", "/api/location", "secret", `{"journeyId":"abc","lat":-33.8688,"lon":151.2093,"accuracy":8,"battery":80,"time":1767261540000}`); rec.Code != http.StatusOK {
		t.Fatalf("location: got %d %s", rec.Code, rec.Body)
	}
	if rec := do("POST", "/api/location?token=secret", "", "lat=-33.87&lon=151.21"); rec.Code != http.StatusOK {
		t.Fatalf("form location: got %d %s", rec.Code, rec.Body)
	}
	if rec := do("POST", "/api/stop", "secret", `{"journeyId":"abc"}`); rec.Code != http.StatusOK {
		t.Fatalf("stop: got %d %s", rec.Code, rec.Body)
	}

	rec := do("GET", "/api/state", "", "")
	var st State
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	j := st.Journey
	if j.ID != "abc" || !j.Ended || len(j.Points) != 2 {
		t.Fatalf("unexpected state: %+v", j)
	}
	if p := j.Points[0]; p.Lat != -33.8688 || p.Battery == nil || *p.Battery != 80 || !p.Time.Equal(time.UnixMilli(1767261540000)) {
		t.Fatalf("unexpected first point: %+v", p)
	}
}
