# lmmcp: thin MCP bridge (slices 1–2)

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
strings or nonnegative integer tokens. EOF drains work; broken output/Ctrl-C cancels.
Each CLI action prints one JSON document; serve prints its address once. Numeric
progress/lifecycle/list_changed events use stderr, never mixed into stdout.

## Configuration and lifecycle

Standard {mcpServers:{name:{command,args,env,cwd}}}; optional envAllowlist,
allowTools/denyTools. Default inherited environment is PATH only; [] inherits none.
Parsed ${NAME} references grant lookup of that name only. Remote entries use url,
headers and type or transport: http/streamable-http (default), sse, auto.
Optional protocolVersion selects 2026-07-28, 2025-11-25 or 2024-11-05.
Headers require ${NAME} references (a literal Bearer prefix is allowed), reject
CR/LF and protocol-reserved names; no argv credentials. URLs reject userinfo,
query credentials and fragments; plaintext requires a literal loopback address.
Redirects and cross-origin legacy endpoint forwarding are refused.

One lazy session/server. SDK StreamableClientTransport/SSEClientTransport own MCP;
reconnect delays are disabled and event ceilings explicitly -1. Auto alone tries
POST initialize then /sse on 4xx, excluding 401/403 (authentication is not discovery).
A session-bearing POST 404 permits one re-initialization and retry; unrelated errors
never replay calls. List changes discard the session and its SDK caches before
next use. The 2025 optional GET notification stream is read asynchronously so a
peer that delays SSE headers cannot block initialization. No retry sleeps, call
deadlines, output quotas or truncation. Teardown requests use our lifetime context,
not the SDK's fixed DELETE deadline; cancellation ends owned reads/connections.

Stdio uses IOTransport MaxLineLength=-1 rather than CommandTransport's fixed
shutdown escalation/frame ceiling. Shutdown closes pipes, terminates/reaps owned
Unix groups or Windows taskkill /T trees. Detached daemons are outside this slice.
Child stderr is discarded; SDK logs suppressed. Typed errors omit upstream text.
Known referenced env values and bearer tokens are blocked even when JSON-escaped;
this is not general DLP. Loopback bind, optional bearer from file/env NAME, strict
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

Tests use SDK fixtures and SHA-256-verified released lmmock v0.1.110: three
transports, JSON/SSE, resources text/blob, prompts, schema/structured results,
cancellation, progress, expiry, auth and changes. Test assertions, mutations,
race/vet, five cross-builds and actual lmscript execution gate this slice locally.
No workflows, public counterpart or release pipeline here. Native platform
qualification and public packaging come next; operator wires real MCP servers.
