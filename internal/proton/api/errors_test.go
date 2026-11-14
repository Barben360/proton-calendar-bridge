package api_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
)

type netErr struct{}

func (netErr) Error() string   { return "network down" }
func (netErr) Timeout() bool   { return true }
func (netErr) Temporary() bool { return true }

var _ net.Error = netErr{}

func TestSessionWide(t *testing.T) {
	t.Parallel()
	apiErr := func(status, code int) error {
		return fmt.Errorf("wrapped: %w", &api.Error{Status: status, Code: code, Message: "x"})
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"session ended", fmt.Errorf("GET /x: %w", api.ErrSessionEnded), true},
		{"exit code error (a refresh failure)", fmt.Errorf("refresh: %w", &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("x")}), true},
		{"429", apiErr(http.StatusTooManyRequests, 0), true},
		{"500", apiErr(http.StatusInternalServerError, 0), true},
		{"503", apiErr(http.StatusServiceUnavailable, 0), true},
		{"network", fmt.Errorf("GET /x: %w", netErr{}), true},
		{"url error", &url.Error{Op: "Get", URL: "u", Err: errors.New("x")}, true},
		{"403", apiErr(http.StatusForbidden, 2011), false},
		{"404", apiErr(http.StatusNotFound, 0), false},
		{"422", apiErr(http.StatusUnprocessableEntity, 2501), false},
		{"499", apiErr(499, 0), false},
		{"plain error", errors.New("decode failed"), false},
	}
	for _, c := range cases {
		if got := api.SessionWide(context.Background(), c.err); got != c.want {
			t.Errorf("%s: SessionWide = %v, want %v", c.name, got, c.want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !api.SessionWide(ctx, errors.New("decode failed")) {
		t.Error("a done context: SessionWide = false, want true")
	}
}
