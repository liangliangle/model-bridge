# Repository Guidelines

## Project Overview

Model Bridge is a large-model API proxy gateway: a single binary with an embedded web UI,
providing protocol conversion, channel failover, MCP relay, and cost tracking.

It accepts three client protocols — OpenAI Chat Completions, OpenAI Responses, and Anthropic
Messages — and routes to channels that speak any of those three.

Two backends implement the same behavior and share the same HTTP contract:

- `src-tauri/` — the original Rust backend (axum, tokio, rusqlite) on `main`.
- `go/` — the Go backend, delivered on `feat/go-override`. It is the one to evolve going forward.

Both serve the same config file (`~/.model-bridge/config.yaml`), the same admin API, and the same
proxy endpoints, so the frontend in `src/` and the macOS client work with either one unchanged.
Where the two disagree, the Rust implementation is the behavior reference: treat it as the oracle.

## Protocol Conversion Matrix

**Read this before touching routing or the converter.** Conversion is deliberately limited to a
single hop, with Chat Completions as the only hub:

| Client (inbound) | Chat channel | Messages channel | Responses channel |
|---|---|---|---|
| Chat (`/v1/chat/completions`) | passthrough | no | no |
| Messages (`/v1/messages`) | convert | passthrough | no |
| Responses (`/v1/responses`) | convert | no | passthrough |

Rule: the channel side must either match the client protocol (byte passthrough) or be a Chat
channel (converted). Flattening Messages/Responses *into* Chat is mechanical; inventing
Messages/Responses semantics *out of* Chat (thinking, cache_control, the Responses item
lifecycle) is where the bugs live, and Messages ↔ Responses would need two chained hops.

Invariants:

- `ApiFormat::can_route_to` in `converter/mod.rs` is the **single source of truth** for this
  matrix. Both routing and the converter consult it — never restate the rule elsewhere.
- `select_routes` (`channel/failover.rs`) hard-filters channels that cannot serve the client
  protocol, **then** orders the survivors purely by `priority`. Filtering outranks priority: a
  Chat client never uses a Messages channel, even at priority 1.
- When no channel can serve the client protocol, the router returns `400` **before dispatch**
  with an explanatory message. Never degrade silently, and never send a request upstream in the
  wrong protocol.
- `FormatConverter::plan` is the checked constructor used by the executor (`from_formats` is the
  lower-level one that assumes the pair is valid). Do not bypass `plan` in production paths.

## Project Structure

- `src/` — React 18 frontend (TypeScript, Tailwind CSS, Vite)
- `src-tauri/` — Rust backend (axum, tokio, rusqlite)
  - `converter/` — protocol conversion: request, response, and streaming state machines.
    Largest module (~4.8k lines); see the matrix above.
  - `proxy/` — `server.rs` (routes), `router.rs` (channel selection, audit, failover loop),
    `executor.rs` (per-channel execution, streaming error detection), `context.rs`
  - `channel/` — channel config (YAML), health tracking / circuit breaker, `failover.rs` routing
  - `audit/` — SQLite audit log. Large request/response bodies live in a separate table and are
    pruned; see `BODIES_RETAIN_LATEST` in `audit/db.rs`.
  - `mcp/` — MCP relay (OAuth 2.1, tool filtering, header injection)
  - `cost/` — per-request cost derived from configured `model_prices`
  - `provider/` — provider adapters and forwarded headers
  - `commands.rs` — admin HTTP API, consumed by the web UI and the macOS client
  - `static_files.rs` — serves the frontend embedded at compile time
- `go/` — Go backend (module `modelbridge`), behavior-compatible with `src-tauri/`
  - `cmd/model-bridge/` — entry point: config load, audit DB, health checker, HTTP listen
  - `internal/converter/` — protocol conversion (largest module; see the matrix above)
  - `internal/proxy/` — `server.go` (routes), `router.go` (selection, audit, failover loop),
    `executor.go` (per-channel execution, streaming error detection), `context.go`
  - `internal/channel/` — health tracking / circuit breaker, `failover.go` routing
  - `internal/audit/` — SQLite audit log; `assemble.go` rebuilds a readable response object
    from a raw SSE stream for the detail view
  - `internal/api/` — the `/api/*` admin handlers
  - `internal/mcp/` — MCP relay (OAuth 2.1, tool filtering, header injection)
  - `internal/cost/`, `internal/config/`, `internal/web/` — pricing, config, embedded frontend
  - `e2e/` — end-to-end tests driving the real HTTP server against a mock upstream
  - `docs/go-rewrite/feature-inventory.md` — the frozen contract inventory the rewrite was built to
- `macos-client/` — SwiftUI menu bar client; talks to the admin HTTP API (independent build)
- `public/` — static assets
- `config.example.yaml` — reference configuration file

## Build & Development Commands

| Command | Description |
|---|---|
| `pnpm dev` | Start Vite dev server on port 3000 (proxies `/api` → `:8080`) |
| `pnpm build` | Build frontend to `dist/` |
| `cd src-tauri && cargo build` | Build Rust backend (debug) |
| `cd src-tauri && cargo test` | Run all tests (115 today) |
| `cd src-tauri && cargo build --release` | Build Rust backend (release) |
| `cd src-tauri && cargo run` | Run backend locally |
| `./build.sh` | Full build: frontend + Go backend → single binary in `release/` (`SKIP_FRONTEND=1` skips the frontend) |
| `./restart.sh` | Stop the running server, rebuild the Go backend, start it in the background (PID in `.model-bridge.pid`, log in `model-bridge.log`) |
| `./build-rust.sh` / `./restart-rust.sh` | The original Rust-only scripts, kept as-is |
| `cd go && ./build.sh` | Sync `dist/` into the Go module and build `go/bin/model-bridge` |
| `cd go && go build -p 1 ./...` | Build the Go backend (serial: this machine has little RAM) |
| `cd go && go test -p 1 ./...` | Run Go unit + end-to-end tests |
| `cd go && ./scripts/verify-startup.sh` | Start the real binary twice and diff its responses |
| `cd go && ./bin/model-bridge` | Run the Go backend locally |

**Build order matters.** `build.rs` embeds `dist/` into the binary via `include_bytes!`, so
`pnpm build` must run *before* `cargo build`. The Go module has the same constraint for a
different reason: `//go:embed` cannot reference files outside the module directory, so
`go/build.sh` copies the repo's `dist/` into `go/internal/web/dist/` before compiling. That copy
is committed so a fresh clone can build without running the frontend build first.

The generated `embedded_assets.rs` records absolute paths, so if the checkout moves,
`cargo test` fails with `couldn't read .../dist/...` — run `touch build.rs` to force the build
script to regenerate. The Go side has no such trap; it only needs the module directory present.

The listen address comes from `listen_host` / `listen_port` in `~/.model-bridge/config.yaml`
(defaults `127.0.0.1:8080`). `./restart.sh` reads the port back out of that file, so it probes the
same address the server actually binds.

The running app serves the UI at `http://localhost:8080` and proxies all three protocol
endpoints: `/v1/chat/completions`, `/v1/messages`, `/v1/responses`.

## Coding Style & Naming

- **TypeScript/React**: 2-space indentation, strict mode enabled (`noUnusedLocals`, `noUnusedParameters`, `noFallthroughCasesInSwitch`). Components use PascalCase (`ChannelConfig.tsx`), utilities use camelCase (`format.ts`).
- **Rust**: Follow standard `rustfmt` conventions. Modules use `snake_case`, types use `PascalCase`. Each domain lives in its own module with a `mod.rs`.
- **Tailwind**: Use utility classes directly in JSX; avoid custom CSS unless unavoidable.

## Testing

Tests run via `cargo test` — **115 tests today**: 35 unit tests inside `src/`, 80 integration
tests in `src-tauri/tests/`.

| File | Covers |
|---|---|
| `tests/test_channel_selection.rs` | protocol matrix, priority ordering, candidate cap |
| `tests/test_responses_flow.rs` | Responses ↔ Chat ↔ Anthropic conversion, streaming, replay fallback |
| `tests/test_stream_usage.rs` | streaming usage in both output directions, cache-token conventions |
| `tests/test_stream_retry.rs` | per-channel retry and cross-channel failover (`#[tokio::test]`) |
| `tests/fixtures/` | request fixtures — tests must not read files outside the repo |

Guidelines:

- Prefer integration tests in `src-tauri/tests/` for conversion and routing behavior; use a
  `#[cfg(test)] mod tests` block for module-private helpers.
- Streaming converters are tested by feeding raw SSE bytes to `process_chunk` and asserting on the
  emitted events. Always terminate the stream (`data: [DONE]`) or call `finalize()`, otherwise the
  close events are never emitted.
- Tests must be hermetic: no `/tmp` scratch files, no network, no dependence on cwd.

No frontend test framework is configured yet; `vitest` is the suggested choice if adding one.

### Go backend

Run with `cd go && go test -p 1 ./...`. Always pass `-p 1` — this environment has ~570 MiB of free
RAM and parallel compilation gets OOM-killed.

| Package | Covers |
|---|---|
| `internal/converter` | request/response/stream mapping for both directions, namespace round-trip, SSE framing at every byte offset |
| `internal/channel` | protocol-matrix filtering, priority ordering, candidate cap, circuit breaker |
| `internal/audit` | rebuilding a readable response object from raw SSE (Anthropic / Chat / Responses) |
| `internal/api` | every admin endpoint against a real config file and a real SQLite DB |
| `e2e` | the real HTTP server driven by a mock upstream: non-stream and stream for all three entries, failover ordering, MCP tool filtering, audit/cost persistence |

Guidelines:

- `e2e` tests must exercise the real path — build the app with `proxy.NewServer`, point channels at
  the mock upstream, and assert on the bytes that reach the upstream and the bytes sent downstream.
  Do not assert on internals or hardcode expected values that restate the implementation.
- Evidence goes to the directory named by `$SCRATCH`; tests skip evidence writes when it is unset
  but still run every assertion.
- `mock_upstream.go` answers in all three protocols and records every request, so a test can prove
  what was actually forwarded (e.g. `stream_options.include_usage`, `model` rewriting).

## Commit & PR Guidelines

- Use [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `refactor:`, `docs:`, `chore:`.
- Commit messages should be concise and descriptive (the project uses Chinese descriptions — follow that convention).
- PRs should describe the change, list affected modules, and reference related issues.

## Configuration

User config lives at `~/.model-bridge/config.yaml`. Refer to `config.example.yaml` for all
available options. Never commit real API keys or secrets.

- `failover.max_failover_channels` caps how many channels any single request may try; `0` means
  unlimited. It is **not** a retry count — per-channel retries are `channels[].retry_count`.
  The legacy key `max_retries` still loads as a serde alias; serialization writes the new name.
