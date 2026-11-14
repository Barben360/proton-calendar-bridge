package service

import (
	"context"
	"time"
)

// accountAPI is the API (and the Agenda) of one account of a Hub, as
// Account (and Agenda) hands it out: each call runs on the account's Local unless the account was removed
// (then a Usage error, "no account"), and Remove waits for the calls in
// flight, ending the long ones (Serve, WatchSync), before it deletes the
// directory. Close is the Hub's: here it does nothing.
type accountAPI struct{ a *hubAccount }

var (
	_ API    = accountAPI{}
	_ Agenda = accountAPI{}
)

func (x accountAPI) Login(ctx context.Context, req LoginRequest) (LoginResult, error) {
	leave, err := x.a.enter()
	if err != nil {
		return LoginResult{}, err
	}
	defer leave()
	return x.a.local.Login(ctx, req)
}

func (x accountAPI) SubmitTOTP(ctx context.Context, code string) (LoginResult, error) {
	leave, err := x.a.enter()
	if err != nil {
		return LoginResult{}, err
	}
	defer leave()
	return x.a.local.SubmitTOTP(ctx, code)
}

func (x accountAPI) Logout(ctx context.Context, req LogoutRequest) (LogoutResult, error) {
	leave, err := x.a.enter()
	if err != nil {
		return LogoutResult{}, err
	}
	defer leave()
	return x.a.local.Logout(ctx, req)
}

func (x accountAPI) Status(ctx context.Context) (Status, error) {
	leave, err := x.a.enter()
	if err != nil {
		return Status{}, err
	}
	defer leave()
	return x.a.local.Status(ctx)
}

func (x accountAPI) Calendars(ctx context.Context) ([]Calendar, error) {
	leave, err := x.a.enter()
	if err != nil {
		return nil, err
	}
	defer leave()
	return x.a.local.Calendars(ctx)
}

func (x accountAPI) Sync(ctx context.Context, req SyncRequest) (SyncResult, error) {
	leave, err := x.a.enter()
	if err != nil {
		return SyncResult{}, err
	}
	defer leave()
	return x.a.local.Sync(ctx, req)
}

func (x accountAPI) WatchSync(ctx context.Context, req SyncRequest, interval time.Duration, onPass func(WatchPass)) error {
	ctx, leave, err := x.a.enterLong(ctx)
	if err != nil {
		return err
	}
	defer leave()
	return x.a.local.WatchSync(ctx, req, interval, onPass)
}

func (x accountAPI) Serve(ctx context.Context, o ServeOptions) error {
	ctx, leave, err := x.a.enterLong(ctx)
	if err != nil {
		return err
	}
	defer leave()
	return x.a.local.Serve(ctx, o)
}

func (x accountAPI) Health(ctx context.Context) (Health, error) {
	leave, err := x.a.enter()
	if err != nil {
		return Health{}, err
	}
	defer leave()
	return x.a.local.Health(ctx)
}

func (x accountAPI) Events(ctx context.Context, req EventsRequest) ([]Event, error) {
	leave, err := x.a.enter()
	if err != nil {
		return nil, err
	}
	defer leave()
	return x.a.local.Events(ctx, req)
}

func (x accountAPI) Collections(ctx context.Context) ([]Collection, error) {
	leave, err := x.a.enter()
	if err != nil {
		return nil, err
	}
	defer leave()
	return x.a.local.Collections(ctx)
}

func (x accountAPI) Objects(ctx context.Context, calID string) ([]ObjectInfo, string, error) {
	leave, err := x.a.enter()
	if err != nil {
		return nil, "", err
	}
	defer leave()
	return x.a.local.Objects(ctx, calID)
}

func (x accountAPI) ObjectsByUID(ctx context.Context, calID string, uids []string) ([]Object, error) {
	leave, err := x.a.enter()
	if err != nil {
		return nil, err
	}
	defer leave()
	return x.a.local.ObjectsByUID(ctx, calID, uids)
}

func (x accountAPI) ObjectsByName(ctx context.Context, calID string, names []string) ([]Object, error) {
	leave, err := x.a.enter()
	if err != nil {
		return nil, err
	}
	defer leave()
	return x.a.local.ObjectsByName(ctx, calID, names)
}

func (x accountAPI) Changes(ctx context.Context, calID, token string) (ChangeSet, error) {
	leave, err := x.a.enter()
	if err != nil {
		return ChangeSet{}, err
	}
	defer leave()
	return x.a.local.Changes(ctx, calID, token)
}

func (x accountAPI) PutObject(ctx context.Context, calID, name string, body []byte, cond Precondition) (PutResult, error) {
	leave, err := x.a.enter()
	if err != nil {
		return PutResult{}, err
	}
	defer leave()
	return x.a.local.PutObject(ctx, calID, name, body, cond)
}

func (x accountAPI) DeleteObject(ctx context.Context, calID, name string, cond Precondition) error {
	leave, err := x.a.enter()
	if err != nil {
		return err
	}
	defer leave()
	return x.a.local.DeleteObject(ctx, calID, name, cond)
}

func (x accountAPI) AddClient(ctx context.Context, name string) (ClientSecret, error) {
	leave, err := x.a.enter()
	if err != nil {
		return ClientSecret{}, err
	}
	defer leave()
	return x.a.local.AddClient(ctx, name)
}

func (x accountAPI) Clients(ctx context.Context) ([]Client, error) {
	leave, err := x.a.enter()
	if err != nil {
		return nil, err
	}
	defer leave()
	return x.a.local.Clients(ctx)
}

func (x accountAPI) RemoveClient(ctx context.Context, idOrName string) (Client, error) {
	leave, err := x.a.enter()
	if err != nil {
		return Client{}, err
	}
	defer leave()
	return x.a.local.RemoveClient(ctx, idOrName)
}

func (x accountAPI) ShowClient(ctx context.Context, idOrName string) (ClientCredentials, error) {
	leave, err := x.a.enter()
	if err != nil {
		return ClientCredentials{}, err
	}
	defer leave()
	return x.a.local.ShowClient(ctx, idOrName)
}

func (x accountAPI) RotateClient(ctx context.Context, idOrName string) (ClientSecret, error) {
	leave, err := x.a.enter()
	if err != nil {
		return ClientSecret{}, err
	}
	defer leave()
	return x.a.local.RotateClient(ctx, idOrName)
}

// CheckClient refuses (401) once the account is removed.
func (x accountAPI) CheckClient(ctx context.Context, password, remote string) (bool, time.Duration) {
	leave, err := x.a.enter()
	if err != nil {
		return false, 0
	}
	defer leave()
	return x.a.local.CheckClient(ctx, password, remote)
}

func (x accountAPI) Occurrences(ctx context.Context, req OccurrencesRequest) (OccurrencePage, error) {
	leave, err := x.a.enter()
	if err != nil {
		return OccurrencePage{}, err
	}
	defer leave()
	return x.a.local.Occurrences(ctx, req)
}

func (x accountAPI) Event(ctx context.Context, ref EventRef) (EventDetail, error) {
	leave, err := x.a.enter()
	if err != nil {
		return EventDetail{}, err
	}
	defer leave()
	return x.a.local.Event(ctx, ref)
}

func (x accountAPI) FreeBusy(ctx context.Context, req OccurrencesRequest) ([]Occurrence, int, error) {
	leave, err := x.a.enter()
	if err != nil {
		return nil, 0, err
	}
	defer leave()
	return x.a.local.FreeBusy(ctx, req)
}

func (x accountAPI) CreateEvent(ctx context.Context, calID string, f EventFields) (EventRef, string, error) {
	leave, err := x.a.enter()
	if err != nil {
		return EventRef{}, "", err
	}
	defer leave()
	return x.a.local.CreateEvent(ctx, calID, f)
}

func (x accountAPI) UpdateEvent(ctx context.Context, ref EventRef, etag string, scope EditScope, f EventFields, dryRun bool) (UpdateResult, error) {
	leave, err := x.a.enter()
	if err != nil {
		return UpdateResult{}, err
	}
	defer leave()
	return x.a.local.UpdateEvent(ctx, ref, etag, scope, f, dryRun)
}

func (x accountAPI) DeleteEvent(ctx context.Context, ref EventRef, etag string, scope EditScope) error {
	leave, err := x.a.enter()
	if err != nil {
		return err
	}
	defer leave()
	return x.a.local.DeleteEvent(ctx, ref, etag, scope)
}

// Close does nothing: the Hub closes its accounts.
func (accountAPI) Close() error { return nil }
