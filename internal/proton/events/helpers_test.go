package events_test

import (
	"errors"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
)

func errorsIsGone(err error) bool { return errors.Is(err, events.ErrGone) }

func apiCode(err error, code int) bool { return api.IsCode(err, code) }
