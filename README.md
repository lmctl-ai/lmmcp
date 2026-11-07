# lmmcp

A small Go bridge from configured MCP stdio servers to REST, JSON-lines REST,
and a CLI. Development uses loopback mock servers only. This private repository
has no CI or release pipeline yet.

Build with Go 1.25 or newer:

```console
go build ./cmd/lmmcp
go vet ./...
go test -race ./...
```

Use an existing `mcpServers` configuration:

```json
{"mcpServers":{"example":{"command":"/absolute/path/to/mcp-server","args":[],"env":{"API_KEY":"${EXAMPLE_KEY}"},"envAllowlist":["PATH"]}}}
```

Commands start upstreams lazily. `servers` only reads configuration; `tools` and
`call` connect when needed. The default inherited environment is PATH alone;
`envAllowlist: []` inherits nothing. Explicit `${NAME}` references read only that
variable. Keep secrets out of literal configuration values. Configure real
servers yourself; the project tests never use real credentials.

```console
lmmcp servers --config config.json
lmmcp tools example --config config.json
lmmcp call example echo --json '{"text":"hello"}' --config config.json
lmmcp call example echo --arg text=hello --config config.json
lmmcp serve --config config.json --listen 127.0.0.1:8080 --token-env BRIDGE_TOKEN
lmmcp tools example --url http://127.0.0.1:8080 --token-env BRIDGE_TOKEN
lmmcp openapi
```

`--arg` supplies string values; use `--json` for typed arguments. Do not combine
them. Client commands print one JSON document. Success is `{ok:true,data:...}`;
failure is `{ok:false,error:{code,message}}`. Exit codes: 0 success, 2 invalid
request/configuration, 3 server/tool/policy/authentication failure, 4 cancellation.
Long-lived `serve` prints its listening address once; progress/lifecycle events
are JSON on stderr. `--version` prints JSON.

REST routes:

| Method | Path | Body |
| --- | --- | --- |
| GET | `/health` | — |
| GET | `/v1/openapi.json` | — |
| GET | `/v1/servers` | — |
| GET | `/v1/servers/{server}/tools` | — |
| POST | `/v1/servers/{server}/tools/{tool}/call` | tool argument object |
| DELETE | `/v1/requests/{id}` | — |

Set `X-Request-ID` to correlate a request and explicitly cancel it. A disconnected
HTTP client or Ctrl-C cancels its MCP call. No implicit call deadline, response
truncation, frame ceiling or retry replay is added. Tool errors omit upstream
error strings. Normal tool results preserve MCP content and schemas.

```console
lmmcp stdio --config config.json
```

In JSON-lines mode each input line is
`{"id":"r1","method":"GET","path":"/v1/servers","query":{},"body":null}`.
Each output line is `{"id":"r1","status":200,"headers":{},"body":...}`.
Requests run concurrently, so correlate replies by id. IDs may also be
nonnegative integer tokens. Cancel with another request line whose method is
DELETE and path `/v1/requests/r1`. EOF drains submitted work; Ctrl-C or broken
output cancels it. This protocol is REST over stdio, not an MCP server transport.

The listener accepts literal loopback addresses only. Optional bearer auth uses
`--token-env NAME` or `--token-file FILE`, never a token argument. Browser Origins,
incorrect Host headers, URL credentials and client redirects are refused.
`--read-only` requires MCP readOnlyHint=true with no destructiveHint=true;
annotations are hints, not a security sandbox. Per-server `allowTools` and
`denyTools` are exact tool names; deny wins and an explicit empty allow list
allows none.

Child stderr is discarded rather than exposing unknown secrets. Known referenced
environment values and the bearer token are blocked from serialized responses;
this is not general data-loss prevention. Only explicitly configured executables
are trusted. Shutdown terminates/reaps owned processes and inherited process
groups; intentionally detached daemons are outside this slice's containment.

Resources/prompts forwarding, remote HTTP/SSE MCP upstreams, an lmcode host and
public releases are subsequent slices. Remote config entries are recognized but
return an explicit unsupported error without connecting. See [design](docs/DESIGN.md).

Reproducible controls live in `scripts/mutations.py` (20 production-copy mutants)
and `scripts/e2e.py` (built CLI plus SDK mock, Linux process-identity checks).
Both accept explicit evidence paths and use only synthetic credentials.
