# Agents (MCP)

How to let an AI agent (Claude Code, or any client that speaks MCP and can send a header) read your calendars and, when you allow it, edit them: on this machine over stdio, or from another machine with a token.

## What an agent gets

`pcb` is an MCP server with eleven tools:

| Tool | Does |
|---|---|
| `status` | the accounts' state, with the current time (agents have no reliable clock) |
| `calendars_list` | the calendars |
| `free_busy` | busy times only |
| `events_list` | events, recurring events expanded |
| `event_get` | one event |
| `events_search` | search events |
| `event_create`, `event_update`, `event_delete` | edit |
| `login`, `logout` | start a login, sign out |

Which of them an agent sees depends on its [grant](#what-an-agent-may-do).

How it was tested: against a fake Proton, and live on 2026-11-14 with Claude Code 2.1.325 against a Proton test account; remote `/mcp` over TLS is not checked live yet ([status](status.md)).

### Text from calendars is untrusted

Text that comes from calendars (titles, descriptions, locations, attendee names) reaches the agent marked `untrusted`, with a note that it is data written by others, never instructions, and invitations from someone else are flagged.

That helps, but your controls are:

- the grants below;
- the ETags: an agent edits only what it just read, and only the fields it names (alarms, exceptions and anything else stay);
- the delete confirmation, which also covers a change that removes future occurrences of a series.

## On this machine (stdio)

The agent starts `pcb mcp`, which speaks MCP on stdin and stdout.

```sh
pcb mcp [--profile full|read-only] [--allow CAPS] [--account A]
```

- **When pcb already runs** (`pcb serve`, the service, `pcb-ui`), it relays the session to it over the control socket. There is no port to set: with `pcb-ui`, the agent still starts `pcb mcp`, so the `pcb` binary must be installed too.
- **Otherwise** it serves your accounts itself for as long as the agent's session lasts. Meanwhile `pcb serve` and `pcb-ui` refuse to start, saying an agent session holds the state: end it first.

- **With the pcb user service installed** and running or restarting, `pcb mcp` never takes its place: it waits up to 30 s for the service and relays to it, else exits 20 asking for `pcb service start`. A service you stopped changes nothing.
- **With a system service installed** (`pcb install --system`), `pcb mcp` does not serve on its own (it would be a second server of the same Proton account): use the [remote `/mcp`](#from-another-machine-remote-with-a-token) with a loopback listen and a token instead.

### Set up Claude Code

```sh
claude mcp add --scope user pcb -- pcb mcp                         # full (the default): read, create, edit, delete, log in and out
claude mcp add --scope user pcb -- pcb mcp --profile read-only     # read, plus starting a login
```

`--scope user` makes `pcb` available in every Claude Code session. Without it, Claude Code keeps the server for the directory you ran the command in only (its `local` scope), and a session started elsewhere does not list it. `claude mcp list` shows what a session sees; restart a running session after adding the server.

To give the server an environment, `-e KEY=value` goes after its name: `-e` takes every following word.

Or in a project's `.mcp.json`:

```json
{"mcpServers": {"pcb": {"command": "pcb", "args": ["mcp"]}}}
```

Give the absolute path of `pcb` when the agent's `PATH` lacks it.

## What a call looks like

### Errors

Every refusal or failure of a tool, including one the arguments cause before the tool runs (a missing or mistyped argument, a value outside the schema), is one result flagged as an error whose text is `{"error": {"class", "message", "hint"}}`:

| `class` | Means | The agent |
|---|---|---|
| `refused` | the call is not acceptable as it is (`reason` names a pcb refusal) | fixes the call, or stops |
| `must-act` | a person must do something first: log in, complete a verification in the Proton web app, start `pcb serve` | tells the person; does not retry |
| `retry` | transient: Proton or the network is slow or down; `retry_after_s` when Proton gave one | retries after a pause |
| `conflict` | the event changed since its etag was read | reads it again with `event_get` |
| `not-found` | no such account, calendar or event (or not visible to this session) | looks again with `status` or a list |

A failure pcb cannot explain is `must-act`, with a hint to read the pcb log: no retry is known to fix it. A read with no account to read (none logged in, or none visible to the session) is `must-act` too, naming how to log in.

### Arguments

The tools' input schemas (`tools/list`) carry the allowed values: `scope` is `occurrence` or `series`; `limit` is 1 to 500 (0 or left out: 100); `start_date` and `end_date` are `YYYY-MM-DD`; `alarms` are at most 10 values, each between -1439 (less than a day after the start, for all-day events) and 10079999 minutes before the start. A call outside them is `refused`.

### Times

- A time with an offset is that instant. A wall clock (`2026-11-14T15:00`) or a date is read in the call's `tz`, else the zone `status` shows, the way pcb reads a time already stored: a time that does not exist (a clock change skips it) takes the offset before the change, so `2026-03-08T02:30` in `America/New_York` is 03:30 EDT; a time that happens twice is the first one, so `2026-10-25T02:30` in `Europe/Paris` is 02:30 summer time.
- A range (`from`, `to`) is at most 366 days and ends after it starts; this is checked before anything else.
- `event_create` takes `start` and `end` (a timed event) or `start_date` and `end_date` (all-day), never both: the call is refused as ambiguous and nothing is written.

### Changing an event

`event_update` with only `start` (or only `start_date`) moves the event: its end moves by the same amount and the event keeps its length. Give `end` (or `end_date`) too to change the length: an end given always wins. A series with exceptions moves whole, its exceptions with it. In a dry run of one occurrence, `before` shows the occurrence as long as the series is.

### Free and busy

`free_busy` gives busy intervals cut at the range asked: an all-day event in a 09:00 to 17:00 query is busy from 09:00 to 17:00. Transparent and cancelled events, and events you have declined (your own `PARTSTAT=DECLINED`, whichever of your addresses it names), are free; the same events are `busy: false` in `events_list`. `skipped` counts each event that cannot be read once.

## What an agent may do

| Profile or `--allow` capability | Tools |
|---|---|
| `freebusy` | `status`, `calendars_list`, `free_busy` (busy times only) |
| `read` (includes `freebusy`) | + `events_list`, `event_get`, `events_search` |
| `write` (includes `read`) | + `event_create`, `event_update` (except a change that removes future occurrences of a series, which needs `delete`) |
| `delete` (needs `write`) | + `event_delete`, and an `event_update` that removes future occurrences of a series ([deletes](#deletes-and-sign-outs-ask-you)) |
| `unconfirmed-delete` (includes `delete`) | the same tools, and a delete, a sign-out or a series change that removes future occurrences goes through even when the client cannot ask you to confirm it ([deletes](#deletes-and-sign-outs-ask-you)) |
| `login` (stdio only) | `login` |
| `manage` (stdio only) | `logout` |
| `--profile full` (default) | all six |
| `--profile read-only` | `read` + `login` |

- `--allow write,delete` replaces the profile.
- The default lets an agent delete events and sign an account out, when pcb can ask you first: the agent's client must support form elicitation, else the call is refused ([deletes](#deletes-and-sign-outs-ask-you)). To allow it without asking, use `--allow write,delete,login,manage,unconfirmed-delete`. For an agent you would not let do either, use `--allow write,login` or `--profile read-only`.
- `--account ADDRESS` (repeatable) limits the session to those accounts.
- Over stdio a profile is a guard rail, not a lock: any program running as you can run `pcb` itself.
### Two settings, the stricter wins

Two settings decide what an agent may do, and both always apply:

| Setting | Decides | Set with |
|---|---|---|
| The agent's grant | what **this agent** may do, on every account | `--profile` or `--allow` where you add the agent, or the token's `--allow` |
| The account's MCP setting | what **any agent** may do with **this account** | `pcb account mcp off\|read-only\|read-write`, or the MCP selector in `pcb-ui` |

An agent gets what both allow:

| Account's MCP setting | Agent with `--profile full` (the default), or a token with `write` | Agent with `--profile read-only` |
|---|---|---|
| Off | does not see the account | does not see the account |
| Read-only | reads; every change is refused | reads |
| Read-write (the default) | reads and edits | reads; it has no tools to edit |

With both defaults an agent reads and edits. To keep agents to reads, set the account to Read-only (every agent) or add the agent with `--profile read-only` (that agent).

- Off hides the account from every agent at once. Its sync keeps running while CalDAV is on.
- Read-only refuses every change, from any profile or token: `event_create`, `event_update`, `event_delete` and `logout` answer `refused` (reason `pcb:account-read-only`), and `calendars_list`, `event_get` and `status` show the account as `read_only`.

## From another machine (remote, with a token)

Remote `/mcp` is off by default. Turn it on, on the CalDAV listener, then give each agent its own token.

In `config.toml`:

```toml
[mcp]
remote = true            # serve /mcp on the [caldav] listener (same port and TLS rule)
# timezone = "Europe/Paris"   # for times given without a zone; empty: this machine's zone
```

Restart pcb, then:

```sh
pcb mcp token add laptop-agent --allow read [--account ADDRESS] [--calendar ID] [--expires 30d]   # prints the token once: pcbm_...
pcb mcp token add editor --allow write,delete,unconfirmed-delete   # may delete without asking you (see below)
pcb mcp token list                     # names, grants (unconfirmed-delete included), last use, expiry; never the tokens
pcb mcp token remove laptop-agent      # it stops working at once
```

- A token is stored as a salted hash only: if you lose it, remove it and add another.
- It can never be granted `login` or `manage`.
- An account or calendar outside its grant does not exist for it.

### Connect the agent

The agent sends `Authorization: Bearer <token>`. Keep the token out of command lines, for example in Claude Code's `.mcp.json` from an environment variable:

```json
{"mcpServers": {"pcb": {"type": "http", "url": "http://127.0.0.1:1080/mcp",
  "headers": {"Authorization": "Bearer ${PCB_MCP_TOKEN}"}}}}
```

The URL is your listener's plus `/mcp`.

### TLS, or an SSH tunnel

On a non-loopback bind pcb serves TLS, with its own certificate unless you set yours (`<state>/tls/cert.pem`, the fingerprint in `pcb status`; [the certificate](thunderbird.md#tls-and-the-certificate)). Either:

- make the agent's runtime trust it: for Node-based clients, `NODE_EXTRA_CA_CERTS` pointing at that file (not checked live yet);
- or keep the listener on loopback and reach it through `ssh -L 1080:127.0.0.1:1080 <server>`.

### What the remote endpoint refuses

| Request | Answer |
|---|---|
| after five bad tokens in a minute from one address (IPv6 counted per /64) | 503 with a `Retry-After` |
| a `Host` that is not a loopback name, one of `[caldav] hostnames` or the address it came to | 403 |
| an `Origin` that differs from `Host` | 403 |

## Log in from an agent

The `login` tool takes a username and nothing else: you type the password, 2FA code, mailbox password (two-password accounts) or CAPTCHA, never the agent.

- **In `pcb-ui`** it opens the add-account wizard with the address filled in.
- **Otherwise** pcb opens a one-time page on `http://127.0.0.1:<port>/login/...` for that username (5 minutes; the first browser to use it keeps it). The agent shows you the link, or your client offers to open it. Claude Code 2.1.325 shows `Open in browser`, then `I'm done, continue`; the agent's answer then says whether the login ended there.
- **Every answer also names** `pcb login --username ADDRESS`, for a `pcb mcp` in a container whose loopback your browser cannot reach.

Limits:

- an agent may start one login a minute per session;
- an account that is already logged in is refused (nothing opens);
- a login you abandon at the 2FA step never signs out an account that was working before it;
- over remote there is no login: an account that needs one shows `needs-login` and the step (`pcb login` on the server, or `pcb-ui`).

## Deletes and sign-outs ask you

When your client supports MCP form elicitation, a delete asks: `Delete the event on <date>: «title»?`. What goes comes first, and the title, which a third party may write, cannot rewrite the question. Declining deletes nothing. Sign-outs ask the same way.

A change to a series that removes occurrences after now is a delete too: an earlier end (`UNTIL`), fewer occurrences (`COUNT`), a sparser rule (daily to weekly), no rule at all, a series that ends earlier than before even with more occurrences (daily for three months instead of weekly for good), or a start postponed past one of its occurrences. pcb counts the occurrences it would remove, from now up to two years ahead, and asks: `Change the series starting on <date> so that 3 of its future occurrences are removed: «title»?` (when what goes lies beyond two years, it says it cannot count them). It needs `delete`, and without a client that asks it is refused unless the agent has `unconfirmed-delete`, as a delete is. A change that keeps every occurrence needs `write` alone and asks nothing: a later end, a denser rule that ends as late, a start moved by less than one repetition (an hour later, a day later in a weekly series, another time zone), a new title. A series that pcb cannot count is treated as losing occurrences. All-day and floating events are compared in UTC, so an occurrence within about half a day of now may not be counted. A dry run (`dry_run`) asks nothing and shows the count as `dropped_occurrences`.

A client without form elicitation (none, or URL only) cannot ask you, so a delete or a sign-out is **refused**, with a hint naming both ways out: use a client that supports form elicitation, or give the agent `unconfirmed-delete`. A delete is refused before the event is read, whatever the etag. `unconfirmed-delete` includes `delete`: give it only to an agent you would let delete (and, over stdio with `manage`, sign an account out) unasked, because an instruction hidden in an invitation can make an agent call `event_delete` as easily as you can. A token created before this option existed has no `unconfirmed-delete`: it keeps working, and its deletes are refused when the client cannot ask; create it again with `--allow ...,unconfirmed-delete` to change that. A client that asks is asked whether or not the grant has it.

Claude Code 2.1.325 asks; other clients are not known.

## Audit

Every tool call is one `mcp call` line in the daemon's log:

| pcb runs as | The log |
|---|---|
| `pcb serve` | its stderr, or the journal for the service |
| `pcb-ui` | `pcb-ui.log` |
| `pcb mcp` serving the session itself | its stderr |

Each line has the tool, how the agent came (`remote`, `relay`, `stdio`), the token name or profile, the accounts, calendars and event UID, the outcome and the duration; never a title, a description, a link or a secret.

- `pcb status` shows `mcp: remote on|off; live sessions: ...` while a daemon runs.
- `pcb doctor` lists expired tokens, and warns when remote is on and no usable token exists, whatever the bind.

Why it works this way: [an MCP adapter over stdio and remote](../spec/decisions/0016-mcp-adapter.md), and the [MCP section of the design spec](../spec/SPEC.md#12-a4--mcp-adapter).
