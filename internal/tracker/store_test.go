package tracker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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

	j := current(t, s)
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

func current(t *testing.T, s *Store) Journey {
	t.Helper()
	js := s.Snapshot().Journeys
	if len(js) == 0 {
		t.Fatal("no journeys")
	}
	return js[len(js)-1]
}

func TestHistoryKeptAcrossJourneys(t *testing.T) {
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
	if err := s.Start("j2"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPoint("j2", Point{Lat: 5, Lon: 5}); err != nil {
		t.Fatal(err)
	}

	// A late retry for the ended journey lands in that journey and does not reopen it.
	if err := s.AddPoint("j1", Point{Lat: 2, Lon: 2}); err != nil {
		t.Fatal(err)
	}
	// Retrying start for an older journey must not create a duplicate.
	if err := s.Start("j1"); err != nil {
		t.Fatal(err)
	}
	// Without an ID, points go to the active journey.
	if err := s.AddPoint("", Point{Lat: 6, Lon: 6}); err != nil {
		t.Fatal(err)
	}

	reloaded, _ := newTestStore(t, path)
	js := reloaded.Snapshot().Journeys
	if len(js) != 2 {
		t.Fatalf("got %d journeys, want 2: %+v", len(js), js)
	}
	if j := js[0]; j.ID != "j1" || !j.Ended || len(j.Points) != 2 {
		t.Fatalf("first journey not kept intact: %+v", j)
	}
	if j := js[1]; j.ID != "j2" || j.Ended || len(j.Points) != 2 {
		t.Fatalf("second journey wrong: %+v", j)
	}

	if _, err := reloaded.Stop(""); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.AddPoint("", Point{Lat: 7, Lon: 7}); err != nil {
		t.Fatal(err)
	}
	js = reloaded.Snapshot().Journeys
	if len(js) != 3 || !js[1].Ended || js[2].Ended || len(js[2].Points) != 1 {
		t.Fatalf("expected a third journey after stop: %+v", js)
	}
}

func TestOpenMigratesSingleJourneyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journey.json")
	legacy := `{"id":"old","startedAt":"2026-01-01T00:00:00Z","ended":true,"points":[{"lat":1,"lon":2,"time":"2026-01-01T00:00:00Z","received":"2026-01-01T00:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ := newTestStore(t, path)
	if err := s.AddPoint("new", Point{Lat: 3, Lon: 4}); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := newTestStore(t, path)
	js := reloaded.Snapshot().Journeys
	if len(js) != 2 || js[0].ID != "old" || len(js[0].Points) != 1 || js[1].ID != "new" {
		t.Fatalf("legacy journey not migrated: %+v", js)
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
	if len(st.Journeys) != 1 {
		t.Fatalf("got %d journeys, want 1", len(st.Journeys))
	}
	j := st.Journeys[0]
	if j.ID != "abc" || !j.Ended || len(j.Points) != 2 {
		t.Fatalf("unexpected state: %+v", j)
	}
	if p := j.Points[0]; p.Lat != -33.8688 || p.Battery == nil || *p.Battery != 80 || !p.Time.Equal(time.UnixMilli(1767261540000)) {
		t.Fatalf("unexpected first point: %+v", p)
	}
}
