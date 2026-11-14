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
- the delete confirmation.

## On this machine (stdio)

The agent starts `pcb mcp`, which speaks MCP on stdin and stdout.

```sh
pcb mcp [--profile read-only|full] [--allow CAPS] [--account A]
```

- **When pcb already runs** (`pcb serve`, the service, `pcb-ui`), it relays the session to it over the control socket.
- **Otherwise** it serves your accounts itself for as long as the agent's session lasts. Meanwhile `pcb serve` and `pcb-ui` refuse to start, saying an agent session holds the state: end it first.

- **With the pcb user service installed** and running or restarting, `pcb mcp` never takes its place: it waits up to 30 s for the service and relays to it, else exits 20 asking for `pcb service start`. A service you stopped changes nothing.
- **With a system service installed** (`pcb install --system`), `pcb mcp` does not serve on its own (it would be a second server of the same Proton account): use the [remote `/mcp`](#from-another-machine-remote-with-a-token) with a loopback listen and a token instead.

### Set up Claude Code

```sh
claude mcp add pcb -- pcb mcp                    # read-only (the default): read, plus starting a login
claude mcp add pcb -- pcb mcp --profile full     # also create, edit, delete, log out
```

To give the server an environment, `-e KEY=value` goes after its name: `-e` takes every following word.

Or in a project's `.mcp.json`:

```json
{"mcpServers": {"pcb": {"command": "pcb", "args": ["mcp", "--profile", "full"]}}}
```

Give the absolute path of `pcb` when the agent's `PATH` lacks it.

## What an agent may do

| Profile or `--allow` capability | Tools |
|---|---|
| `freebusy` | `status`, `calendars_list`, `free_busy` (busy times only) |
| `read` (includes `freebusy`) | + `events_list`, `event_get`, `events_search` |
| `write` (includes `read`) | + `event_create`, `event_update` |
| `delete` (needs `write`) | + `event_delete` |
| `login` (stdio only) | `login` |
| `manage` (stdio only) | `logout` |
| `--profile read-only` (default) | `read` + `login` |
| `--profile full` | all six |

- `--allow write,delete` replaces the profile.
- `--account ADDRESS` (repeatable) limits the session to those accounts.
- Over stdio a profile is a guard rail, not a lock: any program running as you can run `pcb` itself.
- `pcb account mcp off` hides an account from every agent at once, whatever their grants. Its sync keeps running while CalDAV is on.

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
pcb mcp token list                     # names, grants, last use, expiry; never the tokens
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

The `login` tool takes a username and nothing else: you type the password, 2FA code or CAPTCHA, never the agent.

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

A client without elicitation deletes on the `delete` grant alone, so grant `delete` only to an agent you would let delete unasked.

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
- `pcb doctor` lists expired tokens, and warns when remote is on with a non-loopback bind and no usable token.

Why it works this way: [an MCP adapter over stdio and remote](../spec/decisions/0016-mcp-adapter.md), and the [MCP section of the design spec](../spec/SPEC.md#12-a4--mcp-adapter).
