# Repository Guidelines

## Project Overview

Model Bridge is a large-model API proxy gateway: a single binary with an embedded web UI,
providing protocol conversion, channel failover, MCP relay, and cost tracking.

It accepts three client protocols — OpenAI Chat Completions, OpenAI Responses, and Anthropic
Messages — and routes to channels that speak any of those three.

The backend lives in `go/`. It serves `~/.model-bridge/config.yaml`, the admin API, and the proxy
endpoints; the frontend in `src/` and the macOS client talk to it over the same HTTP contract.

History worth knowing: the backend was originally written in Rust under `src-tauri/` and was
rewritten in Go, after which the Rust tree was deleted. Source comments still cite the Rust files
that a given behavior was ported from (e.g. "对应 Rust `src-tauri/src/audit/db.rs`") — those paths
no longer exist in the working tree, but the commit history and the `main` branch still have them,
so `git show <old-commit>:src-tauri/src/...` is how you consult the reference implementation.

## Protocol Conversion Matrix

**Read this before touching routing or the converter.** Every combination is supported: a client
speaking any of the three protocols can be served by a channel speaking any of the three, and the
converter bridges the gap (through Chat Completions, the only hub — at most two hops).

| Client (inbound) | Chat channel | Messages channel | Responses channel |
|---|---|---|---|
| Chat (`/v1/chat/completions`) | passthrough | 1 hop | 1 hop |
| Messages (`/v1/messages`) | 1 hop | passthrough | 2 hops (via Chat) |
| Responses (`/v1/responses`) | 1 hop | 2 hops (via Chat) | passthrough |

**A path, not a yes/no.** `converter.NewPlan(client, channel)` (`internal/converter/plan.go`)
returns the hop chain — `[client]` for passthrough, `[client, channel]` for one hop,
`[client, chat, channel]` for two. Request and response directions are both *folded* from that
plan (`internal/converter/direction.go` dispatches each edge), so a caller never assembles a
direction by hand. This is deliberate: the two `ApiFormat` arguments of a direction are the same
type and used to be swappable, which silently produced wrong-protocol bodies once.

Only six primitive edges exist, and every one of the nine cells is their composition:

| Role | Edges |
|---|---|
| Request (client → channel) | `messages→chat`, `responses→chat`, `chat→messages`, `chat→responses` |
| Response (channel → client) | the same four in the opposite pairing (`chat→messages`, `chat→responses`, `messages→chat`, `responses→chat`) |
| Streaming | `chat→messages`, `chat→responses`, `messages→chat`, `responses→chat` |

Invariants:

- **Protocol does not influence channel choice.** `SelectRoutes` (`internal/channel/failover.go`)
  orders candidates purely by `priority`; it no longer filters by protocol. A Messages client can
  therefore land on a priority-1 Responses channel and pay for a two-hop conversion. Adding a
  protocol filter back would re-introduce the old behaviour — don't, unless the matrix is being
  narrowed on purpose.
- The only routing failure is "no enabled, healthy channel". `RouteUnsupportedProtocol` is kept
  (with its message and unit test) but is currently unreachable.
- `Hop` (`internal/converter/hop.go`) is the only entry point to conversion: build one per
  request from `client.AsClient()` + `channel.AsChannel()` and use it for both directions.
- Streaming is **incremental**: `internal/converter/pipeline.go` chains one stage per hop, each
  translating event by event and flushing. Do not "buffer the whole upstream response and replay
  it" as a shortcut — that was considered and rejected (it kills the point of streaming and
  changes first-byte semantics).
- **Lossy is allowed; silent is not.** Fields the target protocol cannot express either fail the
  request with `*UnsupportedFieldError` (400, naming the field and the target protocol) or are
  dropped/rewritten with a note. Notes are collected on the session, exposed via `Hop.Notes()`,
  and logged once per request as `[convert] … losses=…`. Tests assert `Notes()`, not log output.
  See `README.md` → 「有损清单」 for the known losses (notably thinking signatures).
- When the upstream returns a non-SSE JSON body for a stream request, `Hop.ReplayResponseAsSSE`
  converts it to the client protocol and replays it as that protocol's event stream (the
  degradation path in `streamFromUpstream`).

## Project Structure

- `src/` — React 18 frontend (TypeScript, Tailwind CSS, Vite)
- `go/` — Go backend (module `modelbridge`)
  - `cmd/model-bridge/` — entry point: config load, startup self-checks, audit DB, health
    checker, HTTP listen. **It is the composition root**: it builds the `mux`, registers
    `proxy.Register` (`/v1/*`), `api.Register` (`/api/*`), `mcp.Register` (`/mcp/*`,
    `/oauth/callback`) and the embedded frontend, then wraps it with `proxy.Middleware`. Library
    packages never import each other's routers (`proxy` does not import `api`).
  - `internal/converter/` — protocol conversion (largest module; see the matrix above).
    `hop.go` is the only exported entry point: build one `Hop` per request from
    `client.AsClient()` + `channel.AsChannel()`, then use it for **both** the request and the
    response direction. `plan.go` decides the hop chain, `direction.go` maps each single edge to
    its implementation, and `pipeline.go` chains the streaming stages. Each direction has its own
    file pair: `request.go`/`response.go` (rich → Chat) and `request_reverse.go`/
    `response_reverse.go` (Chat → rich), with `stream.go`/`stream_reverse.go` for streaming.
  - `internal/proxy/` — `server.go` (routes, middleware, `/v1/models`), `router.go` (selection,
    audit, failover loop), `executor.go` (per-channel execution, streaming error detection),
    `context.go`
  - `internal/auth/` — the single definition of both auth rules (`admin_token` for `/api/*`,
    `proxy_tokens` for `/v1/*`) and of the Rust-shaped 401 body
  - `internal/sse/` — the single SSE framing/parsing implementation (`Scan`, `Data`, `Done`)
    used by the converter, the audit assembler, the stream error probe and the e2e helpers
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
| `./build.sh` | Full build: frontend + Go backend → single binary in `release/` (`SKIP_FRONTEND=1` skips the frontend) |
| `./restart.sh` | Stop the running server, rebuild the Go backend, start it in the background (PID in `.model-bridge.pid`, log in `model-bridge.log`) |
| `cd go && ./build.sh` | Sync `dist/` into the Go module and build `go/bin/model-bridge` |
| `cd go && go build -p 1 ./...` | Build the Go backend (serial: this machine has little RAM) |
| `cd go && go test -p 1 ./...` | Run Go unit + end-to-end tests |
| `cd go && ./scripts/verify-startup.sh` | Start the real binary twice and diff its responses |
| `cd go && ./bin/model-bridge` | Run the Go backend locally |

**Build order matters.** `//go:embed` cannot reference files outside the module directory, so
`go/build.sh` copies the repo's `dist/` into `go/internal/web/dist/` before compiling. Run
`pnpm build` first (or let `./build.sh` do it). That copy is committed so a fresh clone can build
without running the frontend build first.

The listen address comes from `listen_host` / `listen_port` in `~/.model-bridge/config.yaml`
(defaults `127.0.0.1:8080`). `./restart.sh` reads the port back out of that file, so it probes the
same address the server actually binds.

The running app serves the UI at `http://localhost:8080` and proxies all three protocol
endpoints: `/v1/chat/completions`, `/v1/messages`, `/v1/responses`.

## Coding Style & Naming

- **TypeScript/React**: 2-space indentation, strict mode enabled (`noUnusedLocals`, `noUnusedParameters`, `noFallthroughCasesInSwitch`). Components use PascalCase (`ChannelConfig.tsx`), utilities use camelCase (`format.ts`).
- **Go**: standard `gofmt` formatting; packages are lowercase single words, exported identifiers PascalCase. Keep the Chinese doc comments that cite the behavior each function was ported from.
- **Tailwind**: Use utility classes directly in JSX; avoid custom CSS unless unavoidable.

## Testing

Run with `cd go && go test -p 1 ./...`. Always pass `-p 1` — this environment has ~570 MiB of free
RAM and parallel compilation gets OOM-killed.

| Package | Covers |
|---|---|
| `internal/converter` | all six single-hop edges (both directions, stream and non-stream), two-hop folding, path planning, namespace round-trip, usage round-trips, SSE framing at every byte offset, `Hop` direction regression |
| `internal/channel` | priority ordering, candidate cap, circuit breaker, "no healthy channel" as the only routing failure |
| `internal/auth` | admin/proxy token rules and the exact 401 body |
| `internal/config` | config save/merge (comments, unknown keys, 0600, atomic) and the exposure self-check |
| `internal/audit` | rebuilding a readable response object from raw SSE (Anthropic / Chat / Responses) |
| `internal/api` | every admin endpoint against a real config file and a real SQLite DB |
| `e2e` | the real HTTP server driven by a mock upstream: **the full 3×3 protocol matrix** (non-stream and stream), failover ordering, first-chunk error failover, non-SSE degradation, MCP tool filtering, audit/cost persistence |

Guidelines:

- `e2e` tests must exercise the real path — assemble the app exactly like `cmd/model-bridge` does
  (see `newBackend` in `e2e/e2e_test.go`), point channels at the mock upstream, and assert on the
  bytes that reach the upstream and the bytes sent downstream. Do not assert on internals or
  hardcode expected values that restate the implementation.
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
- **Saving is a document merge, not a rewrite.** `SaveToFile` parses the file on disk into a YAML
  node tree and merges the in-memory config into it, so comments, key order and hand-written
  unknown keys survive; only changed values are replaced in place. It writes through a temp file +
  `rename` and always ends up `0600` (the file holds API keys and the admin token). A file that
  cannot be parsed is *not* truncated — the merge is skipped, a warning is logged, and a fresh
  document is written. Keep it that way: `internal/config/save_test.go` asserts the raw bytes.
- **Exposure self-check on startup.** When `listen_host` is not loopback (`0.0.0.0`, a LAN/public
  IP, or empty) the server logs a `WARNING:` block naming which surfaces have no auth:
  `/api/*` without `auth.admin_token`, `/v1/*` without `auth.proxy_tokens`, and `/mcp/*` (which
  never checks a token, matching the previous behavior). Set `MODEL_BRIDGE_REQUIRE_AUTH=1` to turn
  those warnings into a refusal to start. Loopback default `127.0.0.1` stays silent.
