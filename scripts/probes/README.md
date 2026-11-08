Hostile-server probes for the wp357 findings. They start `stub.py` (a synthetic loopback Streamable HTTP MCP server)
and a `lmmcp serve` under test, and print what happened. Run with `LMMCP_BIN=/abs/lmmcp python3 -I scripts/probes/p_replay.py`.

- `p_replay.py`: an effectful call must be executed once (`effectExecutions: 1`).
- `p_collateral.py`: an unrelated bad request must not abort a running call (`initializeCount: 1`).
- `p_memory.py`: a hostile notification stream must not grow the bridge's memory (flat `rssMBEachSecond`).
- `p_teardown.py`: the session DELETE reaches the server.
The same behaviours are asserted by Go tests in `internal/bridge/fix358*_test.go`.
