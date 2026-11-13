package cli

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/mcp"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
)

// errStdioOnly is the refusal of a token capability only stdio grants
// (design §5.4).
var errStdioOnly = errors.New("login and manage are stdio only: grant them with pcb mcp --profile")

func (a *app) newMCPToken() *cobra.Command {
	return Group("token", "MCP bearer tokens: which remote agents may use /mcp, and what they may do",
		a.newMCPTokenAdd(), a.newMCPTokenList(), a.newMCPTokenRemove())
}

// tokenStore is the MCP token list of the state directory, once root on
// another user's state is refused (stateOwner).
func (a *app) tokenStore() (*access.TokenStore, error) {
	if err := a.stateOwner(); err != nil {
		return nil, err
	}
	dirs, err := resolveDirs(a.Env)
	if err != nil {
		return nil, err
	}
	return access.NewTokenStore(dirs.State, nil), nil
}

// tokenError gives the token store's refusals their exit code.
func tokenError(err error) error {
	switch {
	case errors.Is(err, access.ErrInvalidTokenName), errors.Is(err, access.ErrInvalidCaps):
		return UsageError(err)
	case errors.Is(err, access.ErrTokenExists):
		return &exitcode.Error{Code: exitcode.Usage, Err: err, Hint: "choose another name, or remove that token first (pcb mcp token remove NAME)"}
	case errors.Is(err, access.ErrTokenNotFound):
		return &exitcode.Error{Code: exitcode.Usage, Err: err, Hint: "see pcb mcp token list"}
	}
	return err
}

// tokenAddData is pcb mcp token add's data: the token, with its secret
// (data.token) once added, never in a plan.
type tokenAddData struct {
	access.Token
	Secret string `json:"token,omitempty"`
	Plan   bool   `json:"plan"`
}

func (a *app) newMCPTokenAdd() *cobra.Command {
	var (
		allow, expires      string
		accounts, calendars []string
		plan                bool
	)
	c := OneOperand(selecting(&cobra.Command{
		Use:   "add NAME",
		Short: "Create an MCP token for one remote agent, shown once",
		Long: "Create a bearer token for one remote agent of /mcp. --allow names what it may do: freebusy, read, " +
			"write, delete (comma-separated; login and manage are stdio only). --account limits it to accounts " +
			"(address or local ID, repeatable; none: every account, including ones added later), --calendar to " +
			"calendar IDs (repeatable; none: every calendar), --expires to a lifetime such as 30d or 12h. The token " +
			"is printed once: pcb keeps only a salted hash, so a lost token is removed and added again. NAME is 1 " +
			"to 64 characters, unique (case-insensitive). --plan shows the token without creating it.",
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := a.tokenRequest(cmd, args[0], allow, expires, accounts, calendars)
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			store, err := a.tokenStore()
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			if plan {
				if err := planToken(store, t); err != nil {
					return Finish(cmd, nil, "", err)
				}
				t.Created = time.Now().UTC().Truncate(time.Second)
				return Finish(cmd, tokenAddData{Token: t, Plan: true},
					fmt.Sprintf("Plan: add MCP token %q: %s.\nPlan only: nothing was changed.", t.Name, grantText(t)), nil)
			}
			secret, err := store.Add(cmd.Context(), t)
			if err != nil {
				return Finish(cmd, nil, "", tokenError(err))
			}
			added := t
			if list, err := store.List(); err == nil {
				for _, l := range list {
					if l.Name == t.Name {
						added = l
					}
				}
			}
			return Finish(cmd, tokenAddData{Token: added, Secret: secret}, tokenAddText(added, secret), nil)
		},
	}), "NAME")
	c.Flags().StringVar(&allow, "allow", "", "what the token may do: `CAPS` comma-separated among freebusy, read, write, delete")
	// --account here is the command's own (repeatable: the token's scope),
	// in place of the root's selector; selecting lets it past the root's check.
	c.Flags().StringArrayVar(&accounts, "account", nil, "an `ACCOUNT` the token may reach, by address or local ID (repeatable; none: every account)")
	c.Flags().StringArrayVar(&calendars, "calendar", nil, "a calendar `ID` the token may reach (repeatable; none: every calendar)")
	c.Flags().StringVar(&expires, "expires", "", "how long the token is valid: a `DURATION` such as 30d, 12h or 90m (none: until removed)")
	c.Flags().BoolVar(&plan, "plan", false, "show the token without creating it")
	return c
}

// tokenRequest is the token add's flags make, checked: the capabilities,
// the lifetime, the name and the accounts (every one must exist, asked of
// the daemon when one answers, else of the in-process accounts).
func (a *app) tokenRequest(cmd *cobra.Command, name, allow, expires string, accounts, calendars []string) (access.Token, error) {
	caps, err := parseCaps(allow)
	if err != nil {
		return access.Token{}, err
	}
	t := access.Token{Name: name, Caps: caps, Accounts: accounts, Calendars: calendars}
	if expires != "" {
		d, err := parseLifetime(expires)
		if err != nil {
			return access.Token{}, err
		}
		e := time.Now().Add(d).UTC().Truncate(time.Second)
		t.Expires = &e
	}
	if err := access.ValidateTokenName(name); err != nil {
		return access.Token{}, UsageError(err)
	}
	for _, c := range calendars {
		if strings.TrimSpace(c) == "" {
			return access.Token{}, UsageError(errors.New("--calendar takes a calendar ID, not an empty value"))
		}
	}
	if err := a.checkAccounts(cmd, accounts); err != nil {
		return access.Token{}, err
	}
	return t, nil
}

// parseCaps reads --allow: comma-separated capabilities among
// access.TokenCaps, duplicates dropped, in the order given.
func parseCaps(allow string) ([]string, error) {
	var caps []string
	for c := range strings.SplitSeq(allow, ",") {
		if c = strings.TrimSpace(c); c != "" && !slices.Contains(caps, c) {
			caps = append(caps, c)
		}
	}
	valid := strings.Join(access.TokenCaps, ", ")
	if len(caps) == 0 {
		return nil, UsageError(fmt.Errorf("--allow needs at least one capability: %s", valid))
	}
	if slices.Contains(caps, "login") || slices.Contains(caps, "manage") {
		return nil, UsageError(errStdioOnly)
	}
	for _, c := range caps {
		if !slices.Contains(access.TokenCaps, c) {
			return nil, UsageError(fmt.Errorf("unknown capability %q for --allow: the valid ones are %s", c, valid))
		}
	}
	// What the server would refuse to serve is refused now (ruling R19):
	// delete without write.
	if _, err := mcp.ParseCaps(strings.Join(caps, ",")); err != nil {
		return nil, &exitcode.Error{Code: exitcode.Usage, Err: err, Hint: "add write to --allow, or drop delete"}
	}
	return caps, nil
}

// parseLifetime reads --expires: a whole number of days (30d) or a Go
// duration (12h, 90m), more than zero.
func parseLifetime(s string) (time.Duration, error) {
	bad := UsageError(fmt.Errorf("invalid value %q for --expires: it takes a lifetime such as 30d, 12h or 90m", s))
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 || n > 36500 {
			return 0, bad
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, bad
	}
	return d, nil
}

// checkAccounts refuses a selector that names no account ("no account X",
// exit 2), asking the daemon when one answers, else the in-process
// accounts. Nothing is opened when there is none to check.
func (a *app) checkAccounts(cmd *cobra.Command, selectors []string) error {
	if len(selectors) == 0 {
		return nil
	}
	for _, s := range selectors {
		if strings.TrimSpace(s) == "" {
			return UsageError(errors.New("--account takes an address or a local ID, not an empty value"))
		}
	}
	var list []service.AccountInfo
	c, err := a.daemon(cmd)
	if err != nil {
		return err
	}
	if c != nil {
		defer func() { _ = c.Close() }()
		if list, err = c.Accounts(cmd.Context()); err != nil {
			return err
		}
	} else {
		h, err := a.hub(cmd)
		if err != nil {
			return err
		}
		defer a.closeHub(h)
		list = h.Accounts()
	}
	for _, s := range selectors {
		if _, err := service.SelectAccount(list, s); err != nil {
			return err
		}
	}
	return nil
}

// planToken is the refusal add would meet for t, without writing: a taken
// name (the store checks the rest the same way at add).
func planToken(store *access.TokenStore, t access.Token) error {
	list, err := store.List()
	if err != nil {
		return err
	}
	for _, o := range list {
		if strings.EqualFold(o.Name, t.Name) {
			return tokenError(fmt.Errorf("%w: %q", access.ErrTokenExists, o.Name))
		}
	}
	return nil
}

// grantText is what t allows, in words.
func grantText(t access.Token) string {
	accounts, calendars, expires := "every account", "every calendar", "never expires"
	if len(t.Accounts) > 0 {
		accounts = "accounts " + strings.Join(t.Accounts, ", ")
	}
	if len(t.Calendars) > 0 {
		calendars = "calendars " + strings.Join(t.Calendars, ", ")
	}
	if t.Expires != nil {
		expires = "expires " + t.Expires.Format(time.RFC3339)
	}
	return fmt.Sprintf("%s on %s, %s; %s", strings.Join(t.Caps, ","), accounts, calendars, expires)
}

// tokenAddText prints the token once, then how to use it.
func tokenAddText(t access.Token, secret string) string {
	return fmt.Sprintf(`MCP token %q added: %s.

Token: %s

It is shown only this once: pcb keeps only a hash of it. The agent sends it as the header
  Authorization: Bearer <the token above>
to /mcp on pcb's listener ([mcp] remote = true).
Remove it with: pcb mcp token remove %s`, t.Name, grantText(t), secret, shellquote.Quote(t.Name))
}

// tokenEntry is one token of pcb mcp token list.
type tokenEntry struct {
	access.Token
	Expired bool `json:"expired,omitempty"`
}

type tokenListData struct {
	Tokens []tokenEntry `json:"tokens"`
}

func (a *app) newMCPTokenList() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the MCP tokens and what they allow (never the tokens themselves)",
		Args:  NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := a.tokenStore()
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			list, err := store.List()
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			now := time.Now()
			entries := make([]tokenEntry, 0, len(list))
			for _, t := range list {
				entries = append(entries, tokenEntry{Token: t, Expired: t.Expires != nil && !now.Before(*t.Expires)})
			}
			return Finish(cmd, tokenListData{entries}, tokenTable(entries), nil)
		},
	}
}

// noTokenText is pcb mcp token list with no token.
const noTokenText = "No MCP token yet.\nHint: run pcb mcp token add NAME --allow read"

func tokenTable(list []tokenEntry) string {
	if len(list) == 0 {
		return noTokenText
	}
	orAll := func(v []string) string {
		if len(v) == 0 {
			return "all"
		}
		return strings.Join(v, ",")
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tALLOWS\tACCOUNTS\tCALENDARS\tCREATED\tLAST USED\tEXPIRES")
	for _, e := range list {
		used, expires := "never", "never"
		if !e.LastUsed.IsZero() {
			used = e.LastUsed.Format(time.RFC3339)
		}
		if e.Expires != nil {
			expires = e.Expires.Format(time.RFC3339)
			if e.Expired {
				expires += " (expired)"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Name, strings.Join(e.Caps, ","), orAll(e.Accounts), orAll(e.Calendars),
			e.Created.Format(time.RFC3339), used, expires)
	}
	_ = w.Flush() // a strings.Builder does not fail
	return strings.TrimSuffix(b.String(), "\n")
}

type tokenRemoveData struct {
	Removed access.Token `json:"removed"`
}

func (a *app) newMCPTokenRemove() *cobra.Command {
	var yes bool
	c := OneOperand(&cobra.Command{
		Use:   "remove NAME",
		Short: "Remove an MCP token: agents using it are refused at once",
		Long: "Remove an MCP token: the next request with it is refused. It asks for a confirmation; --yes gives it " +
			"(without a terminal, or with --non-interactive, --yes is required: exit 10).",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := a.tokenStore()
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			list, err := store.List()
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			i := slices.IndexFunc(list, func(t access.Token) bool { return strings.EqualFold(t.Name, args[0]) })
			if i < 0 {
				return Finish(cmd, nil, "", tokenError(fmt.Errorf("%w %q", access.ErrTokenNotFound, args[0])))
			}
			if !yes {
				if err := a.confirmTokenRemove(cmd, list[i].Name); err != nil {
					return Finish(cmd, nil, "", err)
				}
			}
			gone, err := store.Remove(cmd.Context(), list[i].Name)
			if err != nil {
				return Finish(cmd, nil, "", tokenError(err))
			}
			return Finish(cmd, tokenRemoveData{gone}, fmt.Sprintf("MCP token %q removed: agents using it are refused from now on.", gone.Name), nil)
		},
	}, "NAME")
	c.Flags().BoolVar(&yes, "yes", false, "remove without asking")
	return c
}

// confirmTokenRemove asks the person to type "remove" before the token
// name goes; without a person it fails naming --yes (exit 10).
func (a *app) confirmTokenRemove(cmd *cobra.Command, name string) error {
	if !a.interactive(cmd) {
		return &exitcode.Error{Code: exitcode.MustAct, Err: fmt.Errorf("removing MCP token %q refuses its agents at once: confirm with --yes", name),
			Hint: "pcb mcp token remove " + shellquote.Quote(name) + " --yes"}
	}
	answer, err := a.Term.ReadLine(cmd.Context(), fmt.Sprintf("Remove MCP token %q? Agents using it are refused at once. Type remove to confirm: ", name))
	if err != nil {
		return noAnswer(cmd.Context(), "confirmation", err)
	}
	if strings.TrimSpace(answer) != "remove" {
		return &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("not confirmed: nothing was changed"),
			Hint: "type remove at the prompt, or pass --yes"}
	}
	return nil
}
