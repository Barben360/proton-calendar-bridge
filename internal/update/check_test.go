package update_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/update"
)

const tagPath = "/Barben360/proton-calendar-bridge/releases/tag/"

// releases is a fake github.com answering /releases/latest with status and
// location; it counts the requests and keeps the last one's headers.
type releases struct {
	srv      *httptest.Server
	n        atomic.Int32
	status   int
	location string
	ua       atomic.Value
	cookie   atomic.Bool
}

func newReleases(t *testing.T, status int, location string) *releases {
	t.Helper()
	r := &releases{status: status, location: location}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.n.Add(1)
		r.ua.Store(req.Header.Get("User-Agent"))
		r.cookie.Store(req.Header.Get("Cookie") != "")
		if r.location != "" {
			w.Header().Set("Location", r.srv.URL+r.location)
		}
		w.WriteHeader(r.status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *releases) checker(installed string) update.Checker {
	return update.Checker{URL: r.srv.URL + "/Barben360/proton-calendar-bridge/releases/latest", Installed: installed}
}

func TestLatestReadsTheRedirect(t *testing.T) {
	r := newReleases(t, http.StatusFound, tagPath+"v0.2.0")
	rel, err := r.checker("v0.1.0").Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "v0.2.0" || rel.URL != r.srv.URL+tagPath+"v0.2.0" {
		t.Errorf("Latest = %+v", rel)
	}
	if ua := r.ua.Load(); ua != "pcb" {
		t.Errorf("User-Agent = %q, want pcb (no version)", ua)
	}
	if r.n.Load() != 1 {
		t.Errorf("%d requests, want 1 (the redirect is not followed)", r.n.Load())
	}
}

func TestLatestRefusesOtherAnswers(t *testing.T) {
	for name, c := range map[string]struct {
		status   int
		location string
	}{
		"other repository": {http.StatusFound, "/someone/else/releases/tag/v1.0.0"},
		"pre-release tag":  {http.StatusFound, tagPath + "v1.0.0-rc.1"},
		"not a version":    {http.StatusFound, tagPath + "latest"},
		"nested path":      {http.StatusFound, tagPath + "v1.0.0/extra"},
		"no location":      {http.StatusFound, ""},
		"followed (200)":   {http.StatusOK, tagPath + "v1.0.0"},
		"not found":        {http.StatusNotFound, ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := newReleases(t, c.status, c.location)
			if rel, err := r.checker("v0.1.0").Latest(context.Background()); err == nil {
				t.Errorf("Latest = %+v, want an error", rel)
			}
		})
	}
}

func TestLatestSendsNothingForADevBuild(t *testing.T) {
	r := newReleases(t, http.StatusFound, tagPath+"v0.2.0")
	for _, v := range []string{"dev", "v0.1.0-3-gabc123", "v0.1.0-dirty"} {
		if _, err := r.checker(v).Latest(context.Background()); !errors.Is(err, update.ErrDevBuild) {
			t.Errorf("Latest(%q) error = %v, want ErrDevBuild", v, err)
		}
	}
	if r.n.Load() != 0 {
		t.Errorf("%d requests for dev builds, want 0", r.n.Load())
	}
}

func TestLatestTimesOut(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	t.Cleanup(func() { close(block); srv.Close() })
	c := update.Checker{URL: srv.URL, Installed: "v0.1.0", Client: update.NewClient(50 * time.Millisecond)}
	start := time.Now()
	if _, err := c.Latest(context.Background()); err == nil {
		t.Fatal("Latest succeeded against a server that never answers")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Latest took %v", d)
	}
}

func TestDefaultURL(t *testing.T) {
	if update.ReleasesURL != "https://github.com/Barben360/proton-calendar-bridge/releases/latest" {
		t.Errorf("ReleasesURL = %q", update.ReleasesURL)
	}
}
