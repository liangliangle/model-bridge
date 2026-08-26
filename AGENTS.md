# Repository Guidelines

## Project Overview

Model Bridge is a large model API proxy gateway with channel failover, MCP relay, and skill management. It ships as a single binary with an embedded web UI.

## Project Structure

- `src/` — React 18 frontend (TypeScript, Tailwind CSS, Vite)
- `src-tauri/` — Rust backend (axum, tokio, rusqlite)
  - `proxy/` — API request proxy (routing, execution, streaming)
  - `converter/` — Request/response format conversion (OpenAI ↔ Anthropic)
  - `channel/` — Channel config, health checks, failover
  - `mcp/` — MCP relay (OAuth, tool filtering, header injection)
  - `skill/` — Skill scanning and linking
  - `provider/` — Provider adapters (OpenAI, Anthropic)
  - `audit/` — Audit logging models
- `public/` — Static assets
- `config.example.yaml` — Reference configuration file

## Build & Development Commands

| Command | Description |
|---|---|
| `pnpm dev` | Start Vite dev server on port 3000 (proxies `/api` → `:8080`) |
| `pnpm build` | Build frontend to `dist/` |
| `cd src-tauri && cargo build` | Build Rust backend (debug) |
| `cd src-tauri && cargo build --release` | Build Rust backend (release) |
| `cd src-tauri && cargo run` | Run backend locally |
| `./build.sh` | Full release build: frontend + backend → single binary in `release/` |

The running app serves the UI at `http://localhost:8080` and proxies API requests at `http://localhost:8080/v1/chat/completions`.

## Coding Style & Naming

- **TypeScript/React**: 2-space indentation, strict mode enabled (`noUnusedLocals`, `noUnusedParameters`, `noFallthroughCasesInSwitch`). Components use PascalCase (`ChannelConfig.tsx`), utilities use camelCase (`format.ts`).
- **Rust**: Follow standard `rustfmt` conventions. Modules use `snake_case`, types use `PascalCase`. Each domain lives in its own module with a `mod.rs`.
- **Tailwind**: Use utility classes directly in JSX; avoid custom CSS unless unavoidable.

## Testing

No test framework is currently configured. When adding tests, use `cargo test` for Rust and consider `vitest` for the frontend.

## Commit & PR Guidelines

- Use [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `refactor:`, `docs:`, `chore:`.
- Commit messages should be concise and descriptive (the project uses Chinese descriptions — follow that convention).
- PRs should describe the change, list affected modules, and reference related issues.

## Configuration

User config lives at `~/.model-bridge/config.yaml`. Refer to `config.example.yaml` for all available options. Never commit real API keys or secrets.
