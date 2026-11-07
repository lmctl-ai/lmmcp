# lmmcp slice 1 (2026-11-07)

## Decisions from the references

Build thin on the official Go SDK; no fork. [mcpgate](https://github.com/mokeyish/mcpgate)
reads `mcpServers` and exposes `/{service}` plus `/mcp/config`; these are MCP
transport endpoints rather than one CLI-friendly action per route (`src/main.rs`).
[MCPJungle](https://github.com/MCPJungle/MCPJungle) exposes `/servers`, `/tools`
and `/tools/invoke` with a persistent registry/admin surface (`internal/api/server.go`).
[ContextForge](https://github.com/IBM/mcp-context-forge/blob/main/docs/docs/manage/api-usage.md)
offers catalogs, `/rpc`, REST adaptors and management/auth facilities; this slice
needs no federation, database, UI or token service. Each adds useful discovery,
but our CLI needs a fixed JSON/error/exit contract and cancellation across all adapters.

The [Go SDK](https://github.com/modelcontextprotocol/go-sdk) owns MCP framing,
initialization, schemas, progress and cancellation. Pin source in go.mod/go.sum.
Use IOTransport with MaxLineLength=-1: CommandTransport has implicit 5-second
shutdown escalation and a default frame ceiling. Own process start/kill/reap
instead; no call deadline, retry sleep, response truncation or output quota.

## Contract

One static Go binary. `serve --config FILE --listen 127.0.0.1:8080` exposes
GET `/health`, GET `/v1/openapi.json`, GET `/v1/servers`, GET
`/v1/servers/{server}/tools`, POST `/v1/servers/{server}/tools/{tool}/call`
with a JSON object of tool arguments. Return `{ok:true,data:...}` or
`{ok:false,error:{code,message}}`; preserve full MCP content/schema on success,
map MCP isError to TOOL_ERROR, and suppress upstream error strings.
`X-Request-ID` correlates calls; DELETE `/v1/requests/{id}` cancels an active call.
Progress is a typed stderr event (id/server/progress/total), never mixed into stdout.

`servers`, `tools SERVER`, `call SERVER TOOL --json OBJECT` or repeated `--arg k=v`
use `--url ORIGIN` or `--config FILE` (in process, no listener). --arg values are
literal strings; --json supplies typed values; no mixing, duplicates or implicit coercion.
Each client command prints one JSON document: exit 0 success, 2 invalid request,
3 server/tool failure, 4 cancellation. `openapi` prints the same document served
by the bridge. No interactive prompts.

`stdio --config FILE` serves the SAME dispatcher as JSON lines: request
`{id,method,path,query,body}` → `{id,status,headers,body}`. IDs are nonempty safe
ASCII strings or nonnegative integer tokens. Requests may overlap and replies
are correlated by id; a cancellation request is another line. EOF drains submitted requests; broken output or Ctrl-C cancels
in-flight requests, closes sessions and reaps children. No Scanner frame ceiling.

## Configuration, policy and ownership

Adopt `{mcpServers:{name:{command,args,env,cwd}}}`; accept optional type/name/
description and extensions envAllowlist/allowTools/denyTools. Default inherited
environment is PATH only; explicit [] inherits none. env values support parsed
`${ENV_NAME}` references (explicit permission to read only that name); no discovery
of credentials or full ambient environment inheritance. Literal env values remain
compatible but should contain only nonsecret settings. Remote url/headers entries
are recognized and reported unsupported in slice 1, without contacting them.

Bind literal loopback only. Optional `--token-file` or `--token-env NAME` supplies
Bearer auth; never a token argv value. Reject browser Origins and incorrect Host;
no CORS. CLI refuses credentialed URLs, non-loopback plaintext and redirects.
Logs contain lifecycle/progress fields only. Child stderr is drained and discarded;
no regex-based secret guessing. Known referenced env values/auth tokens are blocked
from serialized responses, including JSON-escaped representations. This is not general DLP.
Deny wins over allow. --read-only requires readOnlyHint=true and no contradictory
destructiveHint; annotations are untrusted hints, not a sandbox or proof of no effects.

One lazy session/server supports concurrent request IDs. No automatic replay after
failure: the failed call returns an error; a subsequent request may start a fresh
session. Shutdown closes pipes, explicitly terminates the owned child/group/tree,
and waits for exit. Unix inherited process groups and Windows taskkill /T cover
normal children; deliberately detached daemons are outside this trusted-command slice.

## Iterations and tests

Slice 1: stdio upstreams, tools list/call, all three adapters, OpenAPI/auth/policy,
owned-process cleanup and cancellation. SDK mock has echo/schema, slow/progress,
destructive, tool-error and crash tools plus a resource/prompt. No real credentials.
Race/integration tests use readiness events, disconnect/Ctrl-C, complete large results,
concurrent calls, malformed input and mutation controls; no test success from import errors.
Cross-compile Linux x64/arm64, macOS x64/arm64 and Windows x64; no workflows/releases.

Next: GET `/v1/servers/{server}/resources`, GET `.../resources/read?uri=...`,
GET `.../prompts`, POST `.../prompts/{prompt}/get` with typed arguments; remote
streamable HTTP/SSE with loopback SDK fixtures; resumable progress API; then a thin
lmcode host (manifest v1 required boolean dry, invalid metadata exit 2 before dispatch,
dry describes calls and starts nothing, result sink/frame and exits 0/2/3/4).
CLI resources/read/prompts/prompt mirror those REST routes when implemented.
The private → public sealed draft/qualification pipeline comes later; create no
public counterpart or pipeline now. Operator alone wires real servers/credentials.
