package cache

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
)

// Info is what Inspect found in a cache file.
type Info struct {
	Exists     bool
	Mode       fs.FileMode
	Schema     int    // PRAGMA user_version
	Account    string // account recorded in meta, "" if none
	QuickCheck string // "ok", or what PRAGMA quick_check (or opening) reported
}

// Inspect reads a cache file without ever creating or changing it. A file
// that is not a usable database is reported through QuickCheck, not as an
// error; an error means the file could not be examined at all. An ended ctx
// stops the queries and is returned as the error (ctx.Err()), never as a
// QuickCheck verdict: the file was not found damaged.
func Inspect(ctx context.Context, path string) (Info, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Info{}, nil
	}
	if err != nil {
		return Info{}, err
	}
	info := Info{Exists: true, Mode: fi.Mode()}
	abs, err := filepath.Abs(path)
	if err != nil {
		return info, err
	}
	// A read-only open of a WAL database creates -wal and -shm. With no
	// -wal there is no writer and nothing pending, so read the file as
	// immutable; with one, those files exist and are not created.
	query := "mode=ro&_pragma=busy_timeout(5000)"
	if _, err := os.Stat(abs + "-wal"); errors.Is(err, fs.ErrNotExist) {
		query = "immutable=1"
	}
	u := url.URL{Scheme: "file", Path: uriPath(abs), RawQuery: query}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return info, err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&info.Schema); err != nil {
		if ctx.Err() != nil {
			return info, ctx.Err()
		}
		info.QuickCheck = err.Error()
		return info, nil
	}
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&info.QuickCheck); err != nil {
		if ctx.Err() != nil {
			return info, ctx.Err()
		}
		info.QuickCheck = err.Error()
		return info, nil
	}
	if info.Schema >= 1 && info.Schema <= schemaVersion {
		err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'account'`).Scan(&info.Account)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			if ctx.Err() != nil {
				return info, ctx.Err()
			}
			info.QuickCheck = err.Error()
		}
	}
	return info, nil
}
