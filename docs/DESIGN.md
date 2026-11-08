# lmmcp: thin MCP bridge

Build thin on the official Go SDK, not a fork. [mcpgate](https://github.com/mokeyish/mcpgate)
uses mcpServers and service transport routes; [MCPJungle](https://github.com/MCPJungle/MCPJungle)
uses /servers, /tools and /tools/invoke; [ContextForge](https://github.com/IBM/mcp-context-forge)
adds federation/admin/REST adaptors. We need fixed JSON actions, discoverable schemas,
CLI exits and cancellation, without a database, UI, credential service or registry.

## Contract

One Go binary. `serve --config FILE --listen 127.0.0.1:8080` serves:
GET /health, /v1/openapi.json, /v1/servers, /v1/servers/{server}/tools;
POST /v1/servers/{server}/tools/{tool}/call (argument object);
GET /v1/servers/{server}/resources, /resources/read?uri=..., /resources/templates;
GET /v1/servers/{server}/prompts; POST /prompts/{prompt} (string argument object).
DELETE /v1/requests/{id} cancels an active X-Request-ID.

`servers`, `tools SERVER`, `call SERVER TOOL`, `resources SERVER`, `read SERVER URI`,
`templates SERVER`, `prompts SERVER`, `prompt SERVER NAME` use either --url ORIGIN
or --config FILE (in process, no listener). --json supplies typed arguments;
repeated --arg k=v supplies literal strings, without mixing or duplicate keys.
`openapi` prints the same OpenAPI 3.1 document served by HTTP.

Success: {ok:true,data:...}; bridge/protocol failure: {ok:false,error:{code,message}}.
An MCP isError result is an application result: HTTP **200**, ok=false,
error.code=TOOL_ERROR, data preserves isError/content/structuredContent. CLI exits
**3** for it. Other exits: 0 success, 2 usage/invalid request, 3 upstream/policy/auth
failure, 4 cancelled. Successful structuredContent is preserved alongside text.
Known-secret protection takes precedence over returning any content.

`stdio --config FILE` serves the same dispatcher as concurrent JSON lines:
{id,method,path,query,body} → {id,status,headers,body}. IDs are nonempty safe ASCII
strings or nonnegative integer tokens. EOF drains work; broken stdout exits 3;
`SIGINT`/`SIGTERM`/`SIGHUP` cancels (exit 4) even when stdin is an open pipe.
Each CLI action prints one JSON document on stdout; serve prints its address once.
Stderr is never used for JSON error responses; numeric progress/lifecycle/list_changed
events use stderr, never mixed into stdout.

## Configuration and lifecycle

Standard {mcpServers:{name:{command,args,env,cwd}}}; optional envAllowlist,
allowTools/denyTools. Default inherited environment is PATH only; [] inherits none.
Parsed ${NAME} references grant lookup of that name only. Remote entries use url,
headers and type or transport: http/streamable-http (default), sse, auto.
Optional protocolVersion selects 2026-07-28, 2025-11-25 or 2024-11-05.
Headers require ${NAME} references (a literal Bearer prefix is allowed), reject
CR/LF and protocol-reserved names; no argv credentials. Unknown keys are refused, so
entries with `timeout`, `autoApprove`, `type:"streamableHttp"` or literal headers
from other clients must be adapted. URLs reject userinfo, query, fragment, a bad
port and NUL; redirects, userinfo or cross-origin endpoint events are refused. Proxy
environment variables are ignored on purpose. Destination policy (checked on the
literal host and again for every resolved address at dial time):

| Destination | Default | `allowPrivateAddresses:false` |
| --- | --- | --- |
| link-local 169.254.0.0/16 and fe80::/10 (cloud metadata), fd00:ec2::254, 0.0.0.0, :: | refused | refused |
| RFC 1918, 100.64.0.0/10, fc00::/7 | https allowed | refused |
| loopback | https allowed; literal-IP http allowed | https/DNS refused; literal-IP http allowed |
| public address or DNS name | https allowed | https allowed |
| other plain http | refused | refused |

One lazy session/server. SDK StreamableClientTransport/SSEClientTransport own MCP;
reconnect delays are disabled and event ceilings explicitly -1. Auto alone tries
POST initialize then /sse on 4xx, excluding 401/403 (authentication is not discovery).
Retry is decided per call from its own trace, never from shared state: a call may be
sent again once, and only if nothing was written (its session had already failed)
or its own POST was answered 404 for the session id. A call whose request was
written is never re-sent. JSON-RPC errors become typed REST errors with `mcpCode`
(404 RESOURCE_NOT_FOUND/PROMPT_NOT_FOUND, 400 INVALID_PARAMS, 502 for internal or
server-defined errors) and never replace the session. A session is replaced only
when it cannot carry work (missing, closed) and never under running calls: it is
retired and closed by its last user. List changes rotate the session on next idle
use. The optional 2025 GET stream is read in fixed slices, keeping only one event's
classification window, so memory is flat however long a line or event is. No retry
sleeps, call deadlines, output quotas or truncation. The session DELETE (and
notifications/cancelled) is sent before the lifetime context ends and has no
deadline; a second shutdown signal aborts it.

Stdio uses IOTransport MaxLineLength=-1 rather than CommandTransport's fixed
shutdown escalation/frame ceiling. Shutdown closes pipes, terminates/reaps owned
Unix groups (plus Linux `Pdeathsig: SIGKILL` on parent death) or Windows taskkill /T trees. Detached daemons are outside this slice.
Child stderr is discarded; SDK logs suppressed. Typed errors omit upstream text.
Known referenced env values and bearer tokens (`>= 8` bytes, across JSON strings and
`json.Number` values) are blocked even when JSON-escaped; shorter non-secret values
are not over-blocked; this is not general DLP. `go.mod` pins tagged `go-sdk` releases
by default and only uses an explicit commit pseudo-version (`v1.8.1-0.20261106153814-df0f29525bc6`)
when required for unreleased transport fixes (`StreamableClientTransport.MaxEventSize`
and non-blocking `2026-07-28` `subscriptions/listen` during `Client.Connect`) until the
next tagged release is published. Loopback bind, optional bearer from file/env NAME, strict
Host/Origin and no CORS. Deny wins; readonly requires readOnlyHint=true and no
contradictory destructiveHint. Annotations are hints, not a sandbox.

## lmcode and verification

--lmcode-schema [--json], --lmcode-signatures, repeatable --lmcode FILE with --config.
Manifest v1 is validated before dispatch and every part loaded before execution.
The pinned Go SDK owns dry, typed null safety, result sinks, ordered builder results
and terminal LMCODE_RETURN. Functions mcp_servers/tools/call/resources/templates/
read/prompts/prompt and an MCPServer wrapper use the same dispatcher. Dry returns
nonsecret action previews and starts no process, connection or env lookup. Host
exit categories follow SDK 0 success/2 metadata/3 load-or-VM/4 application.

Tests use SDK fixtures and SHA-256-verified released lmmock v0.1.111: three
transports, JSON/SSE, resources text/blob, prompts, schema/structured results,
cancellation, progress, expiry, auth and changes. Test assertions, mutations,
race/vet, five cross-builds and actual lmscript execution gate this slice locally.
Public CI runs these gates on main pushes and pull requests, with read-only
permissions, pinned actions and released loopback fixtures, only at the canonical
public destination. No release workflow or release exists yet; native
platform qualification is not claimed. Operator alone wires real MCP servers.

Canonical Go module: `github.com/lmctl-ai/lmmcp`.
