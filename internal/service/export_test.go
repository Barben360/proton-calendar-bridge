package service

import (
	"context"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
)

// SessionClient exposes the stored session's client so tests can tell a
// reused client from a new one.
func (l *Local) SessionClient(ctx context.Context) (*api.Client, error) {
	c, _, _, err := l.session(ctx)
	return c, err
}
