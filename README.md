# lmmcp

One Go binary exposes configured MCP servers through REST, a CLI and JSON-lines
REST over stdio. It supports stdio, Streamable HTTP and legacy HTTP+SSE MCP.
Development and tests use loopback mocks only; the operator configures real servers.
This repository is private for now: no workflows, CI dispatches or releases.

## Quickstart for an LLM or script

Build with Go 1.26 or newer (`CGO_ENABLED=0 go build ./cmd/lmmcp`). Existing
`mcpServers` configurations work. For a local MCP process:

```json
{"mcpServers":{"fixture":{"command":"/absolute/path/to/mcp-server","args":[],"envAllowlist":["PATH"]}}}
```

For the released lmmock MCP example, or another explicitly configured remote:

```json
{"mcpServers":{"fixture":{"url":"http://127.0.0.1:8081/mcp","type":"http","protocolVersion":"2026-07-28"}}}
```

`protocolVersion` is optional. `type` (or `transport`) accepts `http`,
`streamable-http`, `sse` or `auto`. Legacy SSE uses a URL ending in `/sse` and
revision `2024-11-05`. A 2025 Streamable HTTP server can explicitly select
`2025-11-25`. `auto` tries POST initialize, then `/sse` only on a non-auth 4xx;
ordinary `http` never silently falls back to legacy SSE.

```console
lmmcp servers --config config.json
lmmcp tools fixture --config config.json
lmmcp call fixture echo --arg text=hello --config config.json
lmmcp call fixture add --json '{"left":2,"right":3}' --config config.json
lmmcp resources fixture --config config.json
lmmcp read fixture mock://blob --config config.json
lmmcp templates fixture --config config.json
lmmcp prompts fixture --config config.json
lmmcp prompt fixture greet --arg name=Ada --config config.json
lmmcp serve --config config.json --listen 127.0.0.1:8080
```

The same client commands accept `--url http://127.0.0.1:8080` instead of
`--config`. `--arg` supplies literal strings; `--json` supplies typed values.
They cannot be combined. Prompts require string argument values.

```console
curl http://127.0.0.1:8080/v1/servers/fixture/tools
curl --json '{"text":"hello"}' http://127.0.0.1:8080/v1/servers/fixture/tools/echo/call
curl 'http://127.0.0.1:8080/v1/servers/fixture/resources/read?uri=mock%3A%2F%2Fblob'
lmmcp openapi
```

The OpenAPI document is also served at `/v1/openapi.json`. Other routes:
`/health`, `/v1/servers`, `/v1/servers/{server}/resources`,
`/resources/templates`, `/prompts`, and POST `/prompts/{prompt}` with a string
argument object. Resource blobs preserve base64 plus MIME type. Tool results
preserve `structuredContent` beside text content and the discovery `outputSchema`.

Results are `{ok:true,data:...}` or `{ok:false,error:{code,message}}`. MCP `isError`
is HTTP **200** with `ok:false`, `error.code:TOOL_ERROR` and the original MCP
result in `data`; the CLI exits **3**. This lets a caller distinguish tool failure
from transport failure. Known-secret suppression overrides content preservation.
CLI exits: 0 success, 2 invalid request/config, 3 tool/upstream/auth/policy error
or broken stdout, 4 cancellation (`SIGINT`, `SIGTERM`, `SIGHUP`). Each client action
prints one JSON document on stdout; stderr is never used for JSON error output
(progress and lifecycle/list-change events are typed JSON on stderr). `--version` prints JSON.

Use `X-Request-ID` to name an HTTP request; DELETE `/v1/requests/{id}` cancels it.
HTTP disconnect and Ctrl-C propagate MCP cancellation. Session-bearing POST 404
allows one re-initialization retry; a second expiry is `SESSION_EXPIRED`, failed
re-initialization is `SESSION_REINITIALIZE_FAILED`. Other errors do not replay calls.
List changes refresh the session/SDK caches before next idle use; active calls
finish before a session is rotated. An optional 2025 GET
notification stream never blocks initialization waiting for its first event.

## JSON-lines REST and lmcode

`lmmcp stdio --config config.json` reads one request per line:
`{"id":"r1","method":"GET","path":"/v1/servers","query":{},"body":null}`.
It writes `{id,status,headers,body}` replies, correlated by string/nonnegative
integer id; concurrent replies may arrive out of order. Cancel using another
line with DELETE `/v1/requests/r1`. EOF drains work; Ctrl-C/broken output cancels.
This is REST semantics over stdio, not an MCP server endpoint.

The lmcode host accepts `--lmcode-schema [--json]`, `--lmcode-signatures` and
repeatable `--lmcode FILE --config config.json`. Global functions:
`mcp_servers()`, `mcp_tools(server)`, `mcp_call(server,tool,Json)`,
`mcp_resources(server)`, `mcp_templates(server)`, `mcp_read(server,uri)`,
`mcp_prompts(server)`, `mcp_prompt(server,name,Json)`. `mcp_server(name)` returns
an `MCPServer` with matching methods. Json results carry the REST envelope.
Invocation manifest v1, dry, result sinks and terminal LMCODE_RETURN frames use
the pinned Go SDK. All builder parts load before activation. Host exits follow
SDK categories: 0 success, 2 metadata/CLI, 3 load/VM, 4 application failure.
A completed program can inspect a tool-error envelope; return status follows
program completion, while the ordinary `call` CLI exits 3 for `isError`.

[examples/tools.lms](examples/tools.lms) lists tools and calls the echo fixture
through `run_lmcode`. Put your built lmmcp on PATH and pass its configuration:

```console
npm install --prefix /absolute/private/compiler @lmctl-ai/lmctl --no-audit --no-fund
/absolute/private/compiler/node_modules/.bin/lmscript --check examples/tools.lms
/absolute/private/compiler/node_modules/.bin/lmscript examples/tools.lms /absolute/config.json
```

Dry host calls return action previews, start no process or connection, read no
credential env values and write no files. Resource callbacks reject null receivers
with a structured error. Dry does not simulate an MCP server.

## Security and verification

Bind literal loopback only. Optional REST bearer auth uses `--token-env NAME` or
`--token-file FILE`, never a token value in argv. Remote config headers must use
`${NAME}` references, for example `"Authorization":"Bearer ${MOCK_MCP_TOKEN}"`;
no raw credential literals. Reserved MCP headers cannot be overridden. URLs
reject userinfo, query strings and fragments; plaintext requires literal loopback.
Redirects and cross-origin legacy endpoints are refused. Host/Origin checks
prevent browser cross-origin access; no CORS.

Stdio inherits PATH only by default; `envAllowlist: []` inherits nothing. Parsed
`${NAME}` env references read only that variable. Literal env values are for
nonsecret settings. Child stderr is discarded and upstream error strings omitted.
Known referenced env values/bearer tokens of length `>= 8` bytes are blocked from
serialized string and numeric results (shorter values such as `"1"` or `"dev"` are
not treated as secrets so normal outputs are not over-blocked); this is not general DLP. `allowTools`/`denyTools` contain exact names, deny wins,
and empty allow permits none. `--read-only` requires readOnlyHint=true without
destructiveHint=true; annotations are untrusted hints, not a sandbox.

No call deadline, response truncation, frame/event ceiling or retry sleep is
added. SDK automatic reconnect delays are disabled. Shutdown terminates/reaps
owned children and inherited Unix process groups / Windows taskkill trees, and Linux
sets `Pdeathsig: SIGKILL` so children cannot outlive a `SIGKILL` of the parent.
Deliberately detached daemons remain outside this slice's trusted-command scope.

Local gates: `go vet ./...`, `CGO_ENABLED=1 go test -race ./...` (requires a C compiler on `PATH` or `CC`), five CGO-disabled cross-builds.
`go test -tags integration ./...` additionally requires `LMMCP_TEST_LMMOCK` pointing
to the SHA-256-verified lmmock v0.1.110 asset and `LMMCP_TEST_PROGRAM` to its compiled
`examples/mcp/server.lms`. The integration fixture explicitly sets JSON/SSE mode
through `/__mock/reply-mode`; its config `replyMode` alone has no effect.
`scripts/mutations-slice2.py` and `scripts/remote-e2e.py` reproduce adversarial and
actual CLI/lmscript checks. See [short design](docs/DESIGN.md). Public packaging,
release plumbing and native Windows/macOS qualification are separate work.
