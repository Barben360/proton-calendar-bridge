package mcp

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The audit (design §5.8): one info line per tool call, "mcp call" with
// the tool, the entry, the grant's name, the accounts (local IDs) and
// calendars it touched, the event's UID, the outcome (ok or the error
// class) and the duration. Nothing else: never an argument's text, a
// result's content or a secret.

// audit is what a tool handler records for its call's line.
type audit struct {
	accounts, calendars []string
	uid                 string
}

type auditKey struct{}

// auditOf is the call's audit record; a throwaway one outside a call.
func auditOf(ctx context.Context) *audit {
	if a, ok := ctx.Value(auditKey{}).(*audit); ok {
		return a
	}
	return &audit{}
}

func (a *audit) account(id string) {
	if !slices.Contains(a.accounts, id) {
		a.accounts = append(a.accounts, id)
	}
}

func (a *audit) calendar(id string) {
	if !slices.Contains(a.calendars, id) {
		a.calendars = append(a.calendars, id)
	}
}

// auditCalls is the session server's middleware writing the audit line of
// every tools/call, those the SDK refuses before a handler runs included.
// A run that only asks the person (InputRequests, protocol 2026-07-28)
// writes none: its retry with the answer writes the call's one line. On
// older protocols the SDK asks and re-runs the handler inside this
// middleware, which sees the call once.
func (ss *session) auditCalls(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
	return func(ctx context.Context, method string, req sdkmcp.Request) (sdkmcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, req)
		}
		tool := "(unknown)" // a name the agent made up is not logged
		if r, ok := req.(*sdkmcp.CallToolRequest); ok && slices.Contains(ss.tools, r.Params.Name) {
			tool = r.Params.Name
		}
		rec := &audit{}
		began := time.Now()
		res, err := next(context.WithValue(ctx, auditKey{}, rec), method, req)
		if r, ok := res.(*sdkmcp.CallToolResult); err == nil && ok && r != nil && r.InputRequests != nil {
			return res, err // a question to the person: the call completes when retried with the answer
		}
		outcome := "ok"
		if r, ok := res.(*sdkmcp.CallToolResult); err != nil || (ok && r != nil && r.IsError) {
			outcome = classRefused // the SDK's own refusals: unknown tool, invalid arguments
			var te *toolError
			if ok && r != nil && errors.As(r.GetError(), &te) {
				outcome = te.Class
			}
		}
		ss.srv.o.Logger.Info("mcp call", "tool", tool, "entry", string(ss.entry), "grant", ss.grant.Name,
			"account", strings.Join(rec.accounts, ","), "calendar", strings.Join(rec.calendars, ","), "uid", rec.uid,
			"outcome", outcome, "ms", time.Since(began).Milliseconds())
		return res, err
	}
}
