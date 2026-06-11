# livegrep-mcp-server

Give your AI assistant fast, regex-powered code search over your repositories.

This is an [MCP](https://modelcontextprotocol.io/) server that connects an MCP
client (Claude, your editor, etc.) to a livegrep `codesearch` backend. Once
it's running, you can ask your assistant things like _"where do we call
`grpc.Dial`?"_ or _"find files named codesearch"_ and it searches your indexed
code instantly.

```diagram
╭──────────────╮   MCP/stdio   ╭───────────────────╮   gRPC    ╭──────────────╮
│  MCP client  │──────────────▶│ livegrep-mcp-server│─────────▶│  codesearch  │
│ (Claude/IDE) │◀──────────────│   (this program)   │◀─────────│   backend    │
╰──────────────╯               ╰───────────────────╯           ╰──────────────╯
```

## Quick start

Three steps and you're searching. (Run these from the repo root.)

**1. Start a search backend.** The easiest way is Docker — no C++ toolchain
needed:

```sh
docker compose up codesearch          # serves gRPC on localhost:9999
```

**2. Build this server** (a Go binary, builds in seconds):

```sh
bazel build //cmd/livegrep-mcp-server
```

**3. Point your MCP client at it.** Copy the template at the repo root and fill
in your absolute path:

```sh
cp .mcp.json.example .mcp.json
# then edit "command" to your repo's absolute binary path
```

```json
{
  "mcpServers": {
    "livegrep": {
      "command": "/ABSOLUTE/PATH/TO/REPO/bazel-bin/cmd/livegrep-mcp-server/livegrep-mcp-server_/livegrep-mcp-server",
      "args": ["-backend", "localhost:9999"]
    }
  }
}
```

`.mcp.json` is gitignored (it holds machine-specific paths);
[`.mcp.json.example`](../../.mcp.json.example) is the committed template.

> **Heads up:** MCP clients launch servers with a minimal `PATH`, so use the
> **absolute path** to the built binary (not `bazel`/`bazelisk`). After a
> `bazel clean`, rebuild with step 2.

Reload your client's MCP servers and you're ready. Try: _"Using livegrep, list
the indexed repositories."_

## What you can ask

Talk to your assistant naturally — it picks the right tool for you.

| You say…                                                  | Tool used      |
| --------------------------------------------------------- | -------------- |
| "Search the code for `grpc\.Dial`"                        | `code_search`  |
| "Find every TODO comment in the server package"           | `code_search`  |
| "Find files whose path matches `codesearch`"              | `search_files` |
| "What repositories are indexed?"                          | `get_repos`    |

### Example results

**"List the indexed repositories"** →

```
livegrep/livegrep   # the src/ tree (filesystem index)
livegrep            # the whole repo at git HEAD
```

Both point at this repo — they're just two views defined in
[`doc/examples/livegrep/index.json`](../../doc/examples/livegrep/index.json).

**"Search the code for the regex `grpc\.Dial`"** →

| File                               | Line | Content                                                               |
| ---------------------------------- | ---- | -------------------------------------------------------------------- |
| client/test/testutil.go            | 53   | `conn, err := grpc.Dial(addr, grpc.WithInsecure(), grpc.WithBlock())` |
| cmd/livegrep-fetch-reindex/main.go | 363  | `client, err := grpc.Dial(addr, grpc.WithInsecure())`               |
| cmd/livegrep-reload/main.go        | 26   | `client, err := grpc.Dial(addr, grpc.WithInsecure())`               |
| server/backend.go                  | 37   | `client, err := grpc.Dial(addr, dialOpts...)`                       |

**"Find files whose path matches `codesearch`"** →

```
src/codesearch.cc, src/codesearch.h, test/codesearch_test.cc,
web/src/codesearch/codesearch_ui.js, ...
```

## Tools reference

| Tool           | Description                            | Parameters                                                                                                                      |
| -------------- | ------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `code_search`  | Search code by regex pattern          | `query` (required), `repo`, `file`, `case_sensitive` (default false), `max_matches` (default 100), `context_lines` (default 2) |
| `search_files` | Search for files by name/path pattern | `pattern` (required), `repo`, `max_matches` (default 100)                                                                       |
| `get_repos`    | List indexed repositories             | _none_                                                                                                                          |

- `code_search` returns each match with `repo`, `path`, `line`, `content`,
  `match_bounds`, and surrounding `context_before`/`context_after` lines.
- `search_files` returns `repo` and `path`.
- Both include a `stats` block.
- Bad input (missing `query`, invalid regex) comes back as a normal result with
  `isError: true` — the server keeps running.

## Troubleshooting

| Symptom                                              | Cause & fix                                                                                                                                                                  |
| --------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Failed to reconnect … ENOENT`                      | The client can't find the `command`. Use the **absolute** path to the built binary in `.mcp.json` (not `bazel`/`bazelisk`), and build it first.                            |
| `HTTP/2 framing` / `transport` error on a search    | `-backend` is pointing at something that isn't the gRPC backend. Check it's up (`docker compose ps`) and the port isn't taken by another process (`lsof -nP -iTCP:9999 -sTCP:LISTEN`). |
| `connection refused`                                | No backend running. Start one: `docker compose up codesearch`.                                                                                                             |
| Search finds 0 matches for code you just wrote      | The index reads the repo's **git HEAD**, so uncommitted changes aren't searchable yet (the `src/` view does see your working tree).                                        |
| Port `9999` already in use                          | `CODESEARCH_PORT=19999 docker compose up codesearch`, then set `-backend localhost:19999` in `.mcp.json`.                                                                  |

## Development

### Build & test

```sh
bazel build //cmd/livegrep-mcp-server:livegrep-mcp-server
bazel test  //cmd/livegrep-mcp-server:go_default_test
```

Unit tests run against a fake backend (`pb.CodeSearchClient`), so no live
`codesearch` is needed.

### Run the backend with Docker

The repo-root [`docker-compose.yml`](../../docker-compose.yml) uses the public
`livegrep/base` image (which ships the C++ `codesearch` binary) to index this
repo:

```sh
docker compose up codesearch   # backend only, on localhost:9999
docker compose up              # backend + web UI at http://localhost:8910
```

> The public images are `linux/amd64` (emulated on Apple Silicon) and don't
> include `livegrep-mcp-server` — that's why you build it locally.

### Run without a client (smoke test)

The server speaks newline-delimited JSON-RPC 2.0 on stdio — pipe requests
straight in (responses go to stdout, logs to stderr):

```sh
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_repos","arguments":{}}}' \
  | bazel-bin/cmd/livegrep-mcp-server/livegrep-mcp-server_/livegrep-mcp-server -backend localhost:9999 2>/dev/null
```

For a full automated check (starts a backend, runs assertions, tears down), see
[`validate.sh`](validate.sh).
