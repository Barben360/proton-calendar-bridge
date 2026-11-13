package service

import (
	"context"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/store"
)

// Router chooses the account a CalDAV request is for (design §4.4).
// accountID is the local ID the request's URL names, empty on the
// compatibility URLs, where the password alone chooses. The answer is one
// of: api (with localID, its account's local ID) serves the request; api
// nil and retryAfter 0, a refusal (401); api nil and retryAfter > 0, the
// remote is blocked or the account cannot answer now (503 with
// Retry-After); err, the router cannot answer (503).
type Router interface {
	Route(ctx context.Context, accountID, password, remote string) (api API, localID string, retryAfter time.Duration, err error)
}

// AccountPath is the base of the CalDAV URLs of the account localID:
// /dav/<local-id>/.
func AccountPath(localID string) string { return "/dav/" + localID + "/" }

// accountRetry is the wait a request is given when its password is the
// client's of an account that cannot answer: parked, or waiting for a
// login (pcb serve waiting for one answers the same).
const accountRetry = 30 * time.Second

var _ Router = (*Hub)(nil)

// Route implements Router. The shared limiter is consulted once, before any
// password is looked at, and charged once for a request refused 401 on a
// password no account holds. On an account's URLs the password must be
// one of that account's clients; an unknown local ID is a 401 like a wrong
// password. On the compatibility URLs each account is tried. A client of
// an account with CalDAV off is a 401 without a charge, on either kind of
// URL, and is neither touched nor counted as activity (ruling R28): every
// Thunderbird comes from 127.0.0.1, so charging a client left configured
// would soon block every account's. An account whose clients.json (or state.json) cannot be read
// answers 503 with no charge on its URLs; on the compatibility URLs a
// password no other account holds is then a 503, as it may be that
// account's. A matched account that is parked or waiting for a login is a
// 503 with accountRetry. An account being removed is skipped, never
// waited for (ruling R39). The API handed out is the account's as Account
// gives it: a removal waits for its calls in flight.
func (h *Hub) Route(ctx context.Context, accountID, password, remote string) (API, string, time.Duration, error) {
	if h.isClosed() {
		return nil, "", 0, errClosed()
	}
	if ok, wait := h.o.Limiter.Allow(remote); !ok {
		return nil, "", wait, nil
	}
	var candidates []*hubAccount
	if accountID == "" {
		candidates = h.list()
	} else if a := h.byID(accountID); a != nil {
		candidates = []*hubAccount{a}
	}
	broken := false
	for _, a := range candidates {
		found, on, err := h.verify(a, password)
		switch {
		case err != nil:
			broken = true
		case found && !on:
			return nil, "", 0, nil
		case found:
			if h.unavailable(a) {
				return nil, "", accountRetry, nil
			}
			return accountAPI{a}, a.id, 0, nil
		}
	}
	if broken {
		// Not charged, though a password no sound store holds may be a
		// guess (ruling R29): the broken account's own Thunderbird shares
		// the localhost IP with every other, and a charge would lock all
		// of them out for as long as the store stays broken.
		return nil, "", clientsRetry, nil
	}
	h.o.Limiter.Fail(remote)
	return nil, "", 0, nil
}

// byID is the account whose local ID is id, nil if none.
func (h *Hub) byID(id string) *hubAccount {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.accounts[id]
}

// verify reports whether password is a CalDAV client's of a, through its
// gate (a removed account holds none), and whether a has CalDAV on; a
// client of an account with CalDAV off is not used (no touch, no
// activity). err is set when a's clients.json or state.json cannot be
// read. An account being removed holds none either, and is not waited
// for (ruling R39): its removal holds the gate while it revokes the
// session, which would hold every request on the compatibility URLs.
func (h *Hub) verify(a *hubAccount, password string) (found, on bool, err error) {
	a.lmu.Lock()
	removing := a.removing
	a.lmu.Unlock()
	if removing {
		return false, false, nil
	}
	leave, err := a.enter()
	if err != nil {
		return false, false, nil
	}
	defer leave()
	st, err := store.LoadState(a.dir)
	if err != nil {
		return false, false, err
	}
	on = switchOn(st.CalDAV)
	found, err = a.local.verifyClient(password, on)
	return found, on, err
}

// unavailable reports whether a cannot answer CalDAV now: its serve loop
// is parked, or waits for a login.
func (h *Hub) unavailable(a *hubAccount) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return a.stopped != nil || a.state == ServeWaitingForLogin
}
