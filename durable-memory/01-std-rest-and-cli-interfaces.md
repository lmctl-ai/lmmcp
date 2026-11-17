# Standard `lmcode` REST Endpoints & Stateless CLI Interfaces

## Summary

`lmmcp` upgrades `github.com/lmctl-ai/lmcode-go` to `2027887` (`lmcode_std_v3` 226 canonical signatures) and `@lmctl-ai/lmctl@0.2.482` (`scripts/prepare-mock.sh`):
- **Standard Interfaces**: `internal/host/host.go` (`lmcode.NewRegistry()`) registers the 10 standard REST/CLI interfaces (`RestGet`, `RestPost`, `RestPut`, `RestDelete`, `RestPatch`, `RestHead`, `RestOptions`, `RestHandler`, `CliCommand`, `CliOption`).
- **Standard Functions & Methods**: Exposes `rest_config`, `rest_route`, `rest_serve`, `rest_stop`, `rest_cli`, and the 15 `RestExchange` methods alongside `MCPServer` and `mcp_*` functions.
- **Verification**: Covered by `TestStdRestAndCliInterfaces` in `internal/host/host_test.go`, verifying 226 standard signatures and executing `CliOption`, `CliCommand`, `RestGet`, and `rest_cli(...)` calling `mcp_server`, `mcp_servers`, and `mcp_call`.
