package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	proton "github.com/ProtonMail/go-proton-api"

	"github.com/Barben360/proton-calendar-bridge/internal/buildinfo"
)

const (
	defaultBaseURL = "https://mail-api.proton.me"
	maxBody        = 4 << 20
)

// Response is one raw Proton API answer. Code is Proton's own "Code" field
// (1000 = ok), 0 when the body has none.
type Response struct {
	Status int
	Code   int
	Header http.Header
	Body   []byte
	At     time.Time
}

// OK reports HTTP 200 with Proton code 1000.
func (r Response) OK() bool { return r.Status == http.StatusOK && r.Code == 1000 }

// Get makes one authenticated GET with the saved access token. It never
// refreshes: a stale token shows up as the answer (E01 measures exactly that).
// It does not retry; the caller sees Retry-After in Header.
func Get(ctx context.Context, o Options, path string) (Response, error) {
	st, err := load(o.StateDir)
	if err != nil {
		return Response{}, err
	}
	return call(ctx, o, http.MethodGet, path, st.UID, st.AccessToken, nil)
}

// Get is the package-level Get on this session's saved (rotated) tokens.
func (s *Session) Get(ctx context.Context, path string) (Response, error) {
	return Get(ctx, s.opts, path)
}

func call(ctx context.Context, o Options, method, path, uid, access string, body []byte) (Response, error) {
	base := strings.TrimRight(o.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	app := o.AppVersion
	if app == "" {
		app = DefaultAppVersion
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("x-pm-appversion", app)
	req.Header.Set("User-Agent", "pcb-lab/"+buildinfo.Version)
	req.Header.Set("Accept", "application/vnd.protonmail.v1+json")
	if uid != "" {
		req.Header.Set("x-pm-uid", uid)
	}
	if access != "" {
		req.Header.Set("Authorization", "Bearer "+access)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		// The URL carries no secret, but keep the error free of the request.
		return Response{}, fmt.Errorf("%s %s: %w", method, path, unwrapURL(err))
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Response{}, err
	}
	var code struct{ Code int }
	_ = json.Unmarshal(b, &code)
	return Response{Status: resp.StatusCode, Code: code.Code, Header: resp.Header, Body: b, At: o.now()}, nil
}

func unwrapURL(err error) error {
	if ue, ok := err.(interface{ Unwrap() error }); ok && ue.Unwrap() != nil {
		return ue.Unwrap()
	}
	return err
}

// RefreshResult is what one refresh, then one retry with the old refresh
// token, showed.
type RefreshResult struct {
	First, Retry   Response // Retry is zero when the first refresh failed
	Retried        bool
	ExpiresIn      int  // seconds, as returned; 0 when absent
	RefreshRotated bool // the answer carries a different refresh token
	AccessRotated  bool
	OldTokenWorks  bool // the retry with the previous refresh token succeeded
}

type refreshBody struct {
	Code         int
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
	Scope        string
}

// RefreshSaved refreshes the saved session once and saves the new tokens. A
// refused refresh is reported in First (nothing saved), not as an error.
func RefreshSaved(ctx context.Context, o Options) (RefreshResult, error) {
	var res RefreshResult
	st, err := load(o.StateDir)
	if err != nil {
		return res, err
	}
	if o.Recorder != nil {
		o.Recorder.Forbid(st.AccessToken)
		o.Recorder.Forbid(st.RefreshToken)
	}
	first, nb, err := refreshOnce(ctx, o, st.UID, st.RefreshToken)
	if err != nil {
		return res, err
	}
	res.First = first
	if first.Status != http.StatusOK {
		return res, nil
	}
	forbid(o, nb)
	res.ExpiresIn = nb.ExpiresIn
	res.RefreshRotated = nb.RefreshToken != "" && nb.RefreshToken != st.RefreshToken
	res.AccessRotated = nb.AccessToken != "" && nb.AccessToken != st.AccessToken
	if err := saveRotation(o.StateDir, &st, authOf(nb)); err != nil {
		return res, fmt.Errorf("saving the rotated session tokens: %w", err)
	}
	return res, nil
}

// Refresh is RefreshSaved followed by one retry with the previous refresh
// token, to learn whether Proton still honours it. A retry that succeeds is
// saved too, so the file always holds the newest pair. It never logs in.
func Refresh(ctx context.Context, o Options) (RefreshResult, error) {
	st, err := load(o.StateDir)
	if err != nil {
		return RefreshResult{}, err
	}
	oldRefresh := st.RefreshToken
	res, err := RefreshSaved(ctx, o)
	if err != nil || res.First.Status != http.StatusOK {
		return res, err
	}
	retry, rb, err := refreshOnce(ctx, o, st.UID, oldRefresh)
	if err != nil {
		return res, err
	}
	res.Retried, res.Retry = true, retry
	if retry.Status == http.StatusOK {
		forbid(o, rb)
		res.OldTokenWorks = true
		if err := saveRotation(o.StateDir, &st, authOf(rb)); err != nil {
			return res, fmt.Errorf("saving the rotated session tokens: %w", err)
		}
	}
	return res, nil
}

func forbid(o Options, b refreshBody) {
	if o.Recorder != nil {
		o.Recorder.Forbid(b.AccessToken)
		o.Recorder.Forbid(b.RefreshToken)
	}
}

func refreshOnce(ctx context.Context, o Options, uid, refresh string) (Response, refreshBody, error) {
	body, _ := json.Marshal(map[string]string{
		"UID": uid, "RefreshToken": refresh,
		"ResponseType": "token", "GrantType": "refresh_token", "RedirectURI": "https://protonmail.ch",
	})
	r, err := call(ctx, o, http.MethodPost, "/auth/v4/refresh", uid, "", body)
	if err != nil {
		return r, refreshBody{}, err
	}
	var rb refreshBody
	if r.Status == http.StatusOK {
		_ = json.Unmarshal(r.Body, &rb)
	}
	return r, rb, nil
}

func authOf(b refreshBody) proton.Auth {
	return proton.Auth{AccessToken: b.AccessToken, RefreshToken: b.RefreshToken}
}
