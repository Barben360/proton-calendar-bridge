# E19 — The MCP Go SDK: version, names, CGO-freeness, cross-origin, client elicitation

Status: **done for M8** (Task 1: source and release-note audit, a scratch module, and three canary tests in CI). Outcome: **`github.com/modelcontextprotocol/go-sdk` v1.8.0, pinned**, pure Go, Apache-2.0 (MIT for parts not yet relicensed). Feeds A4 (SPEC §12), decision 0016, design `spec/designs/2026-11-13-mcp-design.md` §3, §5.2, §5.5, §5.7. Markers as in E17: `[verified: <test or command>]` = reproduced here (`scratch` = the uncommitted scratch module of this experiment), `[source: file:line]` = read in the module, `[claim]` = a third party's own statement, `[inferred]` = ours, `[assumed → HUMAN-CHECKS 10]` = to confirm in the M8 live step. `SDK/` = `go-sdk@v1.8.0` (tag object eeddd84d, commit 3f3b699b, tagged 2026-09-14).

## Question

Which SDK version, and which exact SDK names do Tasks 9 to 14 build on? Is it pure Go, and what Go does it need? Does its HTTP handler check `Host` and `Origin` itself? Which agent clients declare form and URL elicitation?

## Method

1. Release notes v1.0.0 to v1.8.0 (`gh api repos/modelcontextprotocol/go-sdk/releases`), package docs (`go doc -all`), and the source of `SDK/mcp`, `SDK/auth`, `SDK/jsonrpc`.
2. A scratch module (`e19`, not committed): one server with three tools (`echo`, a `delete` confirmed by form elicitation, a `login` offering a URL elicitation), run over stdio and over streamable HTTP on a unix socket, driven by the SDK client at protocol versions 2026-07-28, 2025-11-25 and 2025-06-18; a message-level relay (in-memory ↔ streamable client on a unix socket); `Host`/`Origin` probes on a loopback TCP listener; `CGO_ENABLED=0 go build`, `go list -deps`, `go version -m`.
3. Claude Code's MCP documentation (`https://code.claude.com/docs/en/mcp`, read 2026-11-13) for the capabilities it declares.
4. The canary tests kept in the repository: `internal/mcp/sdk_e19_test.go`.

## Version

- **v1.8.0** is the newest release (module proxy `@latest`, 2026-11-13); it meets the design's ≥ v1.8.0 [verified: `go list -m -json github.com/modelcontextprotocol/go-sdk@latest`].
- Protocol versions it speaks, newest first: `2026-07-28`, `2025-11-25`, `2025-06-18`, `2025-03-26`, `2024-11-05` (`mcp.SupportedProtocolVersions()`) [source: SDK/mcp/shared.go:58-64].
- v1.7.0 (2026-07-28) brought protocol `2026-07-28`: no `initialize` (a `server/discover` RPC, capabilities in each request's `_meta`), **multi round-trip requests (MRTR, SEP-2322) replacing server-initiated elicitation**, no standalone GET stream; the streamable HTTP handler accepts `2026-07-28` only when `Stateless` [claim: v1.7.0 release notes]. v1.8.0 makes a stateful handler answer a `2026-07-28` request with JSON-RPC error `-32022` so the client renegotiates down, adds `ServerOptions.SupportedProtocolVersions`, `ServerSession.NotifyElicitationComplete`, input size caps [claim: v1.8.0 release notes].
- URL-mode elicitation exists since v1.2.0 (SEP-1036) [claim: v1.2.0 release notes]; ≥ v1.8.0 is the design's call, not a URL-mode need.

## Names for Tasks 9 to 14 (import `sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"`)

### Server and tools

| Use | Name and signature |
|---|---|
| Server | `func NewServer(impl *Implementation, options *ServerOptions) *Server`; `Implementation{Name, Title, Version string, ...}` |
| Options | `ServerOptions{Instructions string; Logger *slog.Logger; Capabilities *ServerCapabilities; KeepAlive time.Duration; GetSessionID func() string; SupportedProtocolVersions []string; ...}`. `Capabilities == nil` advertises `logging`; `&ServerCapabilities{}` advertises nothing beyond what tools add |
| Typed tool | `func AddTool[In, Out any](s *Server, t *Tool, h ToolHandlerFor[In, Out])` |
| Handler | `type ToolHandlerFor[In, Out any] func(_ context.Context, request *CallToolRequest, input In) (result *CallToolResult, output Out, _ error)` |
| Raw tool | `func (s *Server) AddTool(t *Tool, h ToolHandler)`; `type ToolHandler func(context.Context, *CallToolRequest) (*CallToolResult, error)`; `func (s *Server) RemoveTools(names ...string)` |
| Tool | `Tool{Name, Title, Description string; InputSchema, OutputSchema any; Annotations *ToolAnnotations; Meta}` |
| Annotations | `ToolAnnotations{ReadOnlyHint bool; IdempotentHint bool; DestructiveHint *bool; OpenWorldHint *bool; Title string}` |
| Request | `type CallToolRequest = ServerRequest[*CallToolParamsRaw]`; `ServerRequest[P]{Session *ServerSession; Params P; Extra *RequestExtra}`; `CallToolParamsRaw{Name string; Arguments json.RawMessage; InputResponses InputResponseMap; RequestState string}` |
| Result | `CallToolResult{Content []Content; StructuredContent any; IsError bool; InputRequests InputRequestMap; RequestState string; Meta}`; `func (r *CallToolResult) SetError(err error)`; `TextContent{Text string}` |
| Request facts | `func (r *ServerRequest[P]) ClientCapabilities() *ClientCapabilities`, `ClientInfo() *Implementation`, `ProtocolVersion() string`; `RequestExtra{TokenInfo *auth.TokenInfo; Header http.Header; ...}` |
| Middleware | `func (s *Server) AddReceivingMiddleware(middleware ...Middleware)`; `type Middleware func(MethodHandler) MethodHandler`; `type MethodHandler func(ctx context.Context, method string, req Request) (Result, error)` |
| Sessions | `func (s *Server) Connect(ctx context.Context, t Transport, opts *ServerSessionOptions) (*ServerSession, error)`; `func (s *Server) Run(ctx context.Context, t Transport) error`; `func (s *Server) Sessions() iter.Seq[*ServerSession]`; `(*ServerSession).ID() string`, `Close() error`, `Wait() error` |

Typed-handler behaviour [source: SDK/mcp/server.go:365-490]: `In` is unmarshalled and validated against the schema inferred from its `json`/`jsonschema` tags (invalid input → tool error, handler not called); `Out` fills `StructuredContent` and `OutputSchema`, and a JSON text block when `Content` is nil; a returned plain `error` becomes a tool error (`IsError`, text = `err.Error()`), a `*jsonrpc.Error` a protocol error; when the result carries `InputRequests`, `Out` is ignored.

### Elicitation (form and URL) from a tool handler

- **Read the client's modes**: `caps := req.ClientCapabilities()`; nil, or `caps.Elicitation == nil` → no elicitation. `ElicitationCapabilities{Form *FormElicitationCapabilities; URL *URLElicitationCapabilities}`; **neither set means form** (pre-2025-11-25 clients send `{}`) [source: SDK/mcp/server.go:1742-1757]. Read it per request, never from `ServerSession.InitializeParams()`: on `2026-07-28` there is no `initialize` and the capabilities come from the request's `_meta` [source: ServerRequest.ClientCapabilities doc] [verified: scratch, stdio at 2026-07-28 reports form].
- **Ask (the way that works on every version)**: return `&CallToolResult{InputRequests: InputRequestMap{"<id>": &ElicitParams{...}}}` with a zero `Out`; on the next invocation read `req.Params.InputResponses["<id>"].(*ElicitResult)`. On `2026-07-28` the client retries the call with `inputResponses`; on older versions the SDK's server middleware sends `elicitation/create` itself and re-invokes the handler once [source: SDK/mcp/mrtr.go:124-163]. **The handler runs twice** for one confirmed call: everything before the question must be safe to repeat, and the decision (delete or not) is taken only in the second run [verified: `TestE19InputRequestsConfirmOnEveryProtocol`; scratch, 2 invocations at 2026-07-28, 2025-11-25, 2025-06-18].
- `ElicitParams{Mode string /* "form" | "url" */; Message string; RequestedSchema any /* form: flat object schema */; URL string; ElicitationID string /* url */}`; `ElicitResult{Action string /* "accept" | "decline" | "cancel" */; Content map[string]any}` [source: go doc].
- URL mode: same `InputRequests` shape with `Mode: "url"`, `URL`, `ElicitationID` [verified: scratch `login` tool, the client handler saw `mode="url"` and the URL on all three versions]. `func (ss *ServerSession) NotifyElicitationComplete(ctx context.Context, params *ElicitationCompleteParams) error` tells the client the out-of-band step ended (`ElicitationCompleteParams{ElicitationID string}`). `func URLElicitationRequiredError(elicitations []*ElicitParams) error` exists (a protocol error asking for URL elicitations) [source: go doc]; not needed by the design.
- **Do not call** `func (ss *ServerSession) Elicit(ctx context.Context, params *ElicitParams) (*ElicitResult, error)` from a handler: it is refused on a `2026-07-28` session ("return an InputRequests map instead") and it checks the client's modes on `InitializeParams` only [source: SDK/mcp/server.go:1619-1628, 1729-1757].
- `RequestState` must be signed if used ("Unauthenticated servers must encrypt, sign and verify this value") [source: CallToolResult doc]. Leave it empty: the retry carries the same arguments (`event_id`, `etag`). `InputResponses` come from the client host, never from the model's tool arguments [inferred].
- A declined form returns `Action: "decline"`, a dismissed one `"cancel"` [verified: scratch `TestInMemoryDeclined`].

### Streamable HTTP (remote `/mcp`, control-socket `/mcp`)

- `func NewStreamableHTTPHandler(getServer func(*http.Request) *Server, opts *StreamableHTTPOptions) *StreamableHTTPHandler` (an `http.Handler`).
- `StreamableHTTPOptions{Stateless bool; JSONResponse bool; Logger *slog.Logger; EventStore EventStore; SessionTimeout time.Duration; DisableLocalhostProtection bool; CrossOriginProtection *http.CrossOriginProtection /* deprecated */; MaxRequestBodyBytes int64 /* 0 = 4 MiB */; PropagateRequestCancellation bool}`.
- **Stateful (zero value) is required**: a stateful handler answers a newest-protocol client with `2025-11-25` and an `Mcp-Session-Id`, and the client's capabilities reach the tool; a stateless one serves a `2025-11-25` client with a throwaway session in which `req.ClientCapabilities()` is **nil**, so no elicitation at all [verified: `TestE19StreamableStatefulKeepsCapabilities`; scratch, both modes × three versions]. A stateless handler does serve `2026-07-28` clients with MRTR elicitation [verified: scratch].
- `SessionTimeout`: idle sessions closed after that duration (zero: never) — the design's "idle timeout instead of a write timeout" belongs here, plus the `http.Server` timeouts.
- `getServer` is called on **every** POST, GET and DELETE (to read the server's protocol list) and again when a POST opens a session; only the session-opening call's server serves the session [source: SDK/mcp/streamable.go:347-352, 648]. A per-grant `Session(g, e)` that builds a server each call therefore builds one per request: cache by grant, or keep it cheap.
- Session binding: a session remembers `auth.TokenInfoFromContext(ctx).UserID` at creation and refuses (403 "session user mismatch") a later request whose `UserID` differs [source: SDK/mcp/streamable.go:559-575, 709-715]. The context key is unexported [source: SDK/auth/auth.go:80, 121]: only `auth.RequireBearerToken` sets it. To bind sessions to a token, wrap the handler in `auth.RequireBearerToken(verifier, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})` with `type TokenVerifier func(ctx context.Context, token string, req *http.Request) (*auth.TokenInfo, error)` returning `&auth.TokenInfo{UserID: <token name>, Expiration: ...}` (or an error wrapping `auth.ErrInvalidToken` → 401); otherwise a stolen `Mcp-Session-Id` works under any valid token [inferred from source].
- Other SDK refusals before our code runs: `Content-Type` not `application/json` → 415; `Accept` without both `application/json` and `text/event-stream` → 400; body over `MaxRequestBodyBytes` → 413; unknown session → 404 [source: SDK/mcp/streamable.go:316-352, 616-627].

### Host and Origin (design §5.2)

| Request to the SDK handler on a 127.0.0.1 listener | Status | Evidence |
|---|---|---|
| loopback `Host` | 200 | [verified: `TestE19HostCheckedOriginNot`] |
| `Host: evil.example` | **403** | idem |
| `Host: pcb.example:8443` (a configured hostname) | **403** | idem |
| loopback `Host`, `Origin: http://evil.example` | **200** (Origin never checked) | idem |
| `Host: pcb.example:8443`, `DisableLocalhostProtection: true` | 200 | idem |
| unix socket, `Host: evil.example` | 200 (no loopback local address, no check) | [verified: scratch] |
| any of the above behind `http.NewCrossOriginProtection().Handler(h)` | foreign `Origin` → 403 | [verified: scratch] |

- The built-in check runs only when the connection's local address is loopback, and refuses any non-loopback `Host` [source: SDK/mcp/streamable.go:316-326; SDK/internal/util/net.go:12-26]. On a non-loopback bind it does nothing.
- Origin: checked by default in v1.4.1 to v1.5.0 only; off by default since v1.6.0; `CrossOriginProtection` deprecated, v1.8.0 removed the `MCPGODEBUG` flags `enableoriginverification` and `disablelocalhostprotection` [claim: release notes v1.4.0, v1.4.1, v1.6.0, v1.8.0].
- **Consequence for Task 10**: set `DisableLocalhostProtection: true` (else `[caldav] hostnames` on a loopback bind are refused) and implement the plan's own guard in front: `Host` loopback or configured → else 403; a present `Origin` whose host differs from `Host` → 403. For the control socket (Task 12) no check applies and none is needed (peer UID).

### stdio, tests, relay

| Use | Name and signature |
|---|---|
| stdio (session mode) | `&sdkmcp.StdioTransport{MaxLineLength: 0 /* 16 MiB */}`; `server.Run(ctx, &sdkmcp.StdioTransport{})` |
| any reader/writer | `&sdkmcp.IOTransport{Reader io.ReadCloser; Writer io.WriteCloser; MaxLineLength int}` (newline-delimited JSON) |
| tests | `func NewInMemoryTransports() (*InMemoryTransport, *InMemoryTransport)`: connect the server first, then the client |
| test client | `func NewClient(impl *Implementation, options *ClientOptions) *Client`; `ClientOptions{ElicitationHandler func(context.Context, *ElicitRequest) (*ElicitResult, error); Capabilities *ClientCapabilities; ...}` (a handler alone declares `{form:{}}` at ≥ 2025-11-25; set `Capabilities.Elicitation` to declare URL); `func (c *Client) Connect(ctx context.Context, t Transport, opts *ClientSessionOptions) (*ClientSession, error)`; `ClientSessionOptions{ProtocolVersion string}`; `(*ClientSession).CallTool(ctx, *CallToolParams) (*CallToolResult, error)`, `ListTools`, `InitializeResult() *InitializeResult`, `ID() string`, `Close() error`; `CommandTransport{Command *exec.Cmd}` runs a stdio server |
| client over a unix socket | `&sdkmcp.StreamableClientTransport{Endpoint: "http://pcb/mcp", HTTPClient: &http.Client{Transport: &http.Transport{DialContext: <dial "unix" socket>}}, MaxRetries: 0 /* 5 */, DisableStandaloneSSE: bool}`; the host part of `Endpoint` is free [verified: scratch `TestStreamableUnixSocket`, `TestHTTPBinary`] |
| raw messages | `type Transport interface{ Connect(ctx context.Context) (Connection, error) }`; `type Connection interface{ Read(context.Context) (jsonrpc.Message, error); Write(context.Context, jsonrpc.Message) error; Close() error; SessionID() string }`; package `github.com/modelcontextprotocol/go-sdk/jsonrpc`: `Message`, `Request`, `Response`, `ID`, `Error`, `func EncodeMessage(msg Message) ([]byte, error)`, `func DecodeMessage(data []byte) (Message, error)` |

- **Message relay works** (Task 13's "byte bridge vs SDK pair", design §7): two goroutines copying `Read` → `Write` between the stdio side's `Connection` (from `StdioTransport`/`IOTransport.Connect`) and `(&StreamableClientTransport{...}).Connect(ctx)`, closing the other side on error, carried `initialize`, tools and a form elicitation end to end at all three versions; a newest-protocol client was renegotiated to `2025-11-25` through the relay [verified: scratch `TestRawRelay`]. What the relay loses against a real SDK client [source: SDK/mcp/streamable.go:2138-2166, 2428-2467]: the client connection never learns the negotiated version (no `Mcp-Protocol-Version` header after `initialize` on legacy versions; the server accepts that) and opens no standalone GET stream, so only messages sent in a request's context reach the agent (tool results, elicitation; a `tools/list_changed` notification would not). Extra headers (`X-Pcb-Mcp-Grant`) go in through the `http.Client`'s `RoundTripper`.
- `LoggingTransport{Transport Transport; Writer io.Writer}` logs every message: never use it (it would log tool arguments and results) [inferred].

### Versions by entry [inferred from the rows above]

- stdio session mode: the newest client gets `2026-07-28` (MRTR, no `initialize`).
- remote and relayed `/mcp` (stateful): every client ends on ≤ `2025-11-25`.
- Handlers written as above (capabilities per request, `InputRequests`) behave the same on both; `ServerOptions.SupportedProtocolVersions` can narrow stdio to the legacy versions if a difference ever matters.

## Build, dependencies, licences

- `CGO_ENABLED=0 go build` of the scratch server → `ELF 64-bit LSB executable, statically linked`; `go version -m` shows `CGO_ENABLED=0` [verified: scratch]. No third-party module in the tree has `import "C"`; the only cgo files in `go list -deps` are the standard library's `net` and `runtime/cgo`, skipped under `CGO_ENABLED=0`; `segmentio/asm` uses Go assembly (`.s`), not cgo [verified: scratch `go list -deps -f '{{.CgoFiles}}'`, `grep -r 'import "C"'`].
- Minimum Go: **1.25.0** (`go` directive of `SDK/go.mod`; `http.CrossOriginProtection` came with 1.25); this module is on 1.27 [source: SDK/go.mod].
- Modules linked into a binary that imports `mcp` [verified: scratch `go list -deps`; versions as selected in this module's `go.mod`]:

| Module | Licence | New here |
|---|---|---|
| github.com/modelcontextprotocol/go-sdk v1.8.0 (`mcp`, `auth`, `oauthex`, `jsonrpc`, internals) | Apache-2.0; contributions not yet relicensed stay MIT (both texts in its `LICENSE`) | yes |
| github.com/google/jsonschema-go v0.4.3 | MIT | yes |
| github.com/segmentio/encoding v0.5.4 | MIT | yes |
| github.com/segmentio/asm v1.1.3 | MIT | yes |
| github.com/yosida95/uritemplate/v3 v3.0.2 | BSD-3-Clause | yes |
| golang.org/x/oauth2 v0.35.0 (linked through `mcp` → `auth`) | BSD-3-Clause | yes |
| golang.org/x/sync, x/sys, x/time | BSD-3-Clause | no (already required, newer than the SDK asks) |

`golang-jwt/jwt`, `go-cmp` and `x/tools` are in the SDK's `go.mod` but not linked (tests, `auth/extauth`) [verified: scratch `go list -deps`].
- In this repository the SDK is, for now, a test dependency of `internal/mcp` only (`sdk_e19_test.go`); `cmd/pcb` links it from Task 9 on, and `cmd/pcb/arch_test.go` keeps guarding cgo.

## Agent clients and elicitation

| Client | Declares | Evidence |
|---|---|---|
| Claude Code CLI ≥ v2.1.287, on `2025-11-25` and older, to stdio/`http`/`sse`/`ws` servers of the user's MCP configuration | `elicitation: {form: {}, url: {}}` | [claim: code.claude.com/docs/en/mcp, "Elicitation modes Claude Code declares"]; author's machine runs 2.1.323 [verified: `claude --version`]; on the wire [assumed → HUMAN-CHECKS 10] |
| Claude Code on `2026-07-28` (its v2 runtime, TypeScript SDK 2.0) | `{form: {}, url: {}}`, sent in each request's `_meta` | [claim: same page, "MCP client runtimes"]; which runtime and version it picks against `pcb mcp` [assumed → HUMAN-CHECKS 10] |
| Claude Code to claude.ai connectors | bare `elicitation: {}` (form only) | [claim: same page]; not a pcb entry |
| Claude Code, non-interactive (`claude -p`) | dialogs need an interactive terminal (stated for task input requests); an `Elicitation` hook can answer instead | [claim: same page, hooks page]; what a plain elicitation gets under `-p` [assumed → HUMAN-CHECKS 10] |
| Claude Code's VS Code extension | reported to declare URL mode but decline it silently, later narrowed to `{}` | [claim: user bug reports, unverified]; [assumed → HUMAN-CHECKS 10] |
| VS Code (Copilot), Cursor, others | not established | [assumed → HUMAN-CHECKS 10] |

The design already copes with every row: confirmations only when the request's capabilities declare form, the login URL in the result text whatever URL mode does (design §5.5, §5.7). A declared mode that the client then declines shows up as `Action: "decline"` or `"cancel"`: `event_delete` and `logout` treat both as refused, never as consent [inferred].

## Decision

Pin **v1.8.0** (`go.mod`). Build tools with `AddTool` and typed handlers; read capabilities with `req.ClientCapabilities()`; elicit only through `InputRequests` (handlers safe to run twice); serve HTTP with a **stateful** `NewStreamableHTTPHandler`, `DisableLocalhostProtection: true` behind our own Host/Origin guard on the listener, sessions bound to the token through `auth.RequireBearerToken`; relay stdio to the control socket with a message copy between two `Connection`s. Credited in `README.md`.

## Canary tests in this repository

`internal/mcp/sdk_e19_test.go`, red on an SDK bump that changes what the design relies on [verified: each one turned red on a sabotage run — `InputResponses` ignored ("exceeded maximum retries"), handler made stateless (negotiated 2026-07-28, no session ID), localhost protection disabled (foreign Host 200)]:
- `TestE19InputRequestsConfirmOnEveryProtocol`: form confirmation through `InputRequests` at 2026-07-28 and 2025-11-25, two handler runs;
- `TestE19StreamableStatefulKeepsCapabilities`: stateful → 2025-11-25, session ID, capabilities seen; stateless legacy → none;
- `TestE19HostCheckedOriginNot`: the Host/Origin table above.

## Open

- HUMAN-CHECKS 10: what Claude Code declares and which version it negotiates against `pcb mcp` (session and relay) and remote `/mcp`; what `-p` does with an elicitation; other clients if used.
- A newer SDK (v1.9.0 removes `plaintextstatefulrejection`, `blockingcancelnotify`): re-run the canary tests and this page's tables before bumping.
