package update

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ReleasesURL redirects to the newest stable release's page (GitHub's
// "latest" skips drafts and pre-releases).
const ReleasesURL = "https://github.com/Barben360/proton-calendar-bridge/releases/latest"

// tagPrefix is the path the redirect must point at, followed by the tag.
const tagPrefix = "/Barben360/proton-calendar-bridge/releases/tag/"

// timeout bounds one check.
const timeout = 10 * time.Second

// ErrDevBuild is the check of a version that is not a release: nothing is
// sent.
var ErrDevBuild = errors.New("dev build")

// Release is the newest stable release: its tag and its page.
type Release struct{ Version, URL string }

// Checker reads the newest stable release. URL is ReleasesURL when empty,
// Client NewClient(10 s) when nil; Installed is the running version.
type Checker struct {
	URL       string
	Client    *http.Client
	Installed string
}

// NewClient is the check's own client: it follows no redirect, keeps no
// cookie and gives up after d.
func NewClient(d time.Duration) *http.Client {
	return &http.Client{
		Timeout:       d,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Latest asks for the newest stable release. It sends one request with a
// bare "pcb" User-Agent (no version, no system), and accepts only a redirect
// to this repository's tag page of a vX.Y.Z tag.
func (c Checker) Latest(ctx context.Context) (Release, error) {
	if !Checkable(c.Installed) {
		return Release{}, ErrDevBuild
	}
	client := c.Client
	if client == nil {
		client = NewClient(timeout)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(), nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("User-Agent", "pcb")
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, err
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return Release{}, fmt.Errorf("GitHub answered %s, not a redirect to the latest release", resp.Status)
	}
	loc, err := resp.Location()
	if err != nil {
		return Release{}, errors.New("GitHub's redirect has no location")
	}
	return c.release(loc)
}

// url is the page asked: URL, else ReleasesURL.
func (c Checker) url() string {
	if c.URL == "" {
		return ReleasesURL
	}
	return c.URL
}

// release is the Release loc names: a page on the asked page's own scheme
// and host (https://github.com in production), at this repository's tag
// page of a vX.Y.Z tag. Anything else is refused, so neither a redirect
// nor update.json can make pcb-ui open another site.
func (c Checker) release(loc *url.URL) (Release, error) {
	base, err := url.Parse(c.url())
	if err != nil {
		return Release{}, err
	}
	tag, ok := strings.CutPrefix(loc.Path, tagPrefix)
	if loc.Scheme != base.Scheme || loc.Host != base.Host || !ok || !stable.MatchString(tag) {
		return Release{}, fmt.Errorf("GitHub redirected to %s, not a release page", redact(loc))
	}
	return Release{Version: tag, URL: loc.String()}, nil
}

// redact is u without its query, for an error message.
func redact(u *url.URL) string {
	v := *u
	v.RawQuery, v.Fragment = "", ""
	return v.String()
}
