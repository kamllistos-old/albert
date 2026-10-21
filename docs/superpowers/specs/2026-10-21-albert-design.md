# Albert — Design Spec

Date: 2026-10-21
Repo: `github.com/kamllistos/albert`
Status: Approved design, pending implementation plan

## 1. Purpose

Albert is a lightweight macOS tool that shows every developer tool and app installed through package managers (Homebrew, uv, pipx, pip, npm, …) in one place. It detects newly installed tools automatically, shows which ones are outdated, and lets the user update one tool or all of them, or install new ones. It offers both a CLI and a web GUI served from a Docker container. It must be usable by non-technical users: two commands, `albert up` and `albert down`, open and close it.

### Success criteria

- `albert up` opens a browser showing all installed tools from every available provider within a few seconds (outdated status may arrive later).
- Installing a tool in any terminal (e.g. `brew install jq`) makes it appear in the open GUI without a manual refresh.
- Outdated tools are flagged; per-tool Update and "Update all" work from both GUI and CLI.
- New tools can be searched and installed from the GUI and CLI.
- `albert down` leaves nothing running.
- Missing package managers are skipped silently; a failing provider never breaks the others.

### Non-goals (v1)

- macOS notifications, launchd autostart / always-on mode
- Uninstall
- Linux or Windows support
- Homebrew tap (deferred; needs a second repo)

## 2. Architecture

A Docker container on macOS runs in a Linux VM and cannot inspect or run the host's macOS package managers. Albert therefore splits into:

- **Host binary `albert` (Go)** — does all real work. Contains the CLI, the providers, and the agent (HTTP API) started by `albert up`.
- **GUI container `albert-ui`** — Caddy on Alpine serving a static web UI and reverse-proxying `/api` to the host agent at `host.docker.internal`.

```
Mac host
  albert (Go binary)
    CLI: list / outdated / update / install / search / up / down / status / doctor
    agent (only while `albert up`): HTTP API on 127.0.0.1:<port>, token auth
      providers (brew, cask, uv, pipx, pip, npm, pnpm, bun, mas, cargo, go, gem)
      fsnotify watcher
      cache: ~/.albert/cache.json, state: ~/.albert/state.json
                 ^
                 | host.docker.internal:<agent port>
Docker
  albert-ui (caddy:alpine): static UI on localhost:7879, /api -> agent
Browser -> http://localhost:7879
```

CLI commands other than `up`/`down`/`status` call providers directly and do not need the agent or Docker.

**Native fallback:** if Docker is not installed or not running, `albert up` explains this and offers to serve the same UI natively from the binary (UI files embedded via `go:embed`). `albert up --native` forces this mode.

## 3. Components

### 3.1 Runner (`internal/runner`)

Thin wrapper over `os/exec` with context timeouts and captured stdout/stderr, behind an interface so providers can be tested with a fake runner. Supports streaming output line-by-line for jobs.

### 3.2 Providers (`internal/provider`)

One file per source. Common interface:

```go
type Tool struct {
    Name, Source, Version, Latest, Description, Homepage, Path string
    Outdated, New, ReadOnly bool
}

type Provider interface {
    Name() string
    Available() bool
    List(ctx context.Context) ([]Tool, error)
    Outdated(ctx context.Context) ([]Update, error)
    Upgrade(ctx context.Context, names []string, out io.Writer) error // empty names = all
    Install(ctx context.Context, name string, out io.Writer) error
    Search(ctx context.Context, q string) ([]Result, error) // may return ErrUnsupported
    WatchPaths() []string
}
```

| Provider | List | Outdated | Upgrade | Install | Search | Watch paths |
|---|---|---|---|---|---|---|
| brew (formulae) | `brew info --json=v2 --installed` | `brew outdated --json=v2` | `brew upgrade <names>` | `brew install` | `brew search --formula` / formulae.brew.sh | `$(brew --prefix)/Cellar` |
| cask | same JSON, casks section | `brew outdated --cask --greedy-auto-updates --json=v2` | `brew upgrade --cask` | `brew install --cask` | `brew search --cask` | `$(brew --prefix)/Caskroom` |
| uv | `uv tool list --show-paths` | compare with PyPI JSON API | `uv tool upgrade <n>` / `--all` | `uv tool install` | PyPI exact-name lookup | `~/.local/share/uv/tools` |
| pipx | `pipx list --json` | compare with PyPI JSON API | `pipx upgrade` / `upgrade-all` | `pipx install` | PyPI exact-name lookup | `~/.local/pipx/venvs` |
| pip | `pip list --format=json` (+ `--user`) | `pip list --outdated --format=json` | `--user` only, with warning | not offered | — | user site-packages |
| npm | `npm ls -g --json --depth=0` | `npm outdated -g --json` | `npm install -g <n>@latest` | `npm install -g` | npm registry search API | `$(npm root -g)` |
| pnpm | `pnpm ls -g --json` | `pnpm outdated -g --format json` | `pnpm update -g` | `pnpm add -g` | npm registry | pnpm global dir |
| bun | `bun pm ls -g` | npm registry compare | `bun update -g` | `bun add -g` | npm registry | `~/.bun/install/global` |
| mas | `mas list` | `mas outdated` | `mas upgrade` | `mas install <id>` | `mas search` | `/Applications` |
| cargo | `cargo install --list` | crates.io API compare | `cargo install <n>` | `cargo install` | crates.io API | `~/.cargo/bin` |
| go | scan `$(go env GOPATH)/bin` + `go version -m` | proxy.golang.org `@latest` | `go install <module>@latest` | `go install <module>@latest` | — | `$(go env GOPATH)/bin` |
| gem | `gem list --local` (user gems) | `gem outdated` | `gem update <n>` | `gem install --user-install` | `gem search` | gem user dir |

Exact flags and JSON shapes are verified against the installed tool versions during implementation; parsers are tested against captured fixtures.

**pip safety:** Homebrew/system Python is PEP 668 "externally managed". pip packages are listed `ReadOnly` unless installed with `--user`. "Update all" excludes pip unless the user explicitly includes it.

### 3.3 Inventory + cache (`internal/inventory`)

Runs `List` for all available providers in parallel (30s timeout each), then `Outdated` in the background (120s timeout each). Merges into one inventory, marks tools not present in the previous cache as `New`, persists to `~/.albert/cache.json`. Exposes change notifications to the agent.

### 3.4 Watcher (`internal/watch`)

fsnotify on each provider's `WatchPaths()`. Events are debounced 2s per provider and trigger a rescan of that provider only. Missing paths are skipped.

### 3.5 Jobs (`internal/jobs`)

Update/install operations run as background jobs with an ID, status (running/success/failed), and streamed output. At most one job per provider at a time; further jobs for that provider queue. After a job finishes, that provider is rescanned.

A job that fails because it needs `sudo` (detected from output, e.g. some cask upgrades) is reported with the message: "This needs your password. Run `albert update <tool>` in Terminal."

### 3.6 Agent API (`internal/agent`)

Binds `127.0.0.1` only. A random token is generated on every `albert up`, passed to the container, and required on every request (`Authorization: Bearer`). Requests without a valid token get 401. Caddy injects the header so the browser never handles it; in native mode the UI is served same-origin and the token is set as an HttpOnly, `SameSite=Strict` cookie.

Because the token is injected server-side, the token alone does not stop a malicious web page from sending requests to `localhost:7879`. The agent (and Caddy) therefore also enforce, on every `/api` request:

- `Host` must be `localhost:<ui port>` or `127.0.0.1:<ui port>` (blocks DNS rebinding).
- If an `Origin` header is present it must match the UI origin.
- State-changing requests (`POST`) must carry `X-Albert-Request: 1`; cross-origin pages cannot set custom headers without a CORS preflight, and no CORS headers are ever sent.

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/tools` | full inventory + provider status |
| POST | `/api/refresh` | rescan all (or `?source=`) |
| POST | `/api/update` | `{source, names[]}` or `{all: true, includePip: bool}` → job IDs |
| POST | `/api/install` | `{source, name}` → job ID |
| GET | `/api/search?q=` | merged results from providers supporting search |
| GET | `/api/jobs/{id}` | job status + output |
| GET | `/api/events` | SSE stream: inventory changes, job output, job completion |

The outdated check repeats every 6h while the agent runs.

### 3.7 Docker control (`internal/dock`)

Uses the `docker` CLI (no SDK dependency). `up`: ensure image exists (pull `ghcr.io/kamllistos/albert-ui:<version>`, else build locally from the Dockerfile embedded in the binary), then `docker run -d --rm --name albert-ui -p 127.0.0.1:7879:80 -e ALBERT_AGENT=host.docker.internal:<port> -e ALBERT_TOKEN=<token>`. `down`: `docker stop albert-ui`.

### 3.8 CLI (`cmd/albert`)

```
albert up [--native] [--no-open]
albert down
albert status
albert list [--source S] [--outdated] [--json]
albert outdated [--source S]
albert update <tool> [--source S] | --all [--source S] [--include-pip]
albert install <tool> [--source S]
albert search <query>
albert doctor
```

`up` starts the agent as a detached background process (writes PID, port, token, mode to `~/.albert/state.json`), starts the container, waits for health, and opens the browser. `down` stops the container, kills the agent PID, and clears state; it still cleans up when the agent has already crashed. If the default ports are busy, the next free ports are used and recorded in state.

When `install`/`update` names a tool present in more than one source and no `--source` is given, the CLI asks which one.

Error messages are written for non-technical users (e.g. "Docker isn't running. Open Docker Desktop, or press Enter to open Albert without it.").

### 3.9 Web UI (`web/`)

Single page, plain modern JavaScript (ES modules) plus one CSS file, no framework and no build step (target < 100KB total). Embedded in the binary and copied into the container image.

- Header: search box, "Update all (N)" button, refresh button.
- Source filter chips with counts; providers that are unavailable shown greyed with reason on hover.
- Table: name, source, installed version, latest version, status badge (up to date / update available / new / read-only), description, Update button.
- Row click opens detail panel: description, homepage, install path.
- "Install new" tab: search across providers, results with source and Install button.
- Job drawer: live output of running jobs, success/failure state.
- Live updates via SSE; "checking for updates…" badge while outdated checks run.

## 4. Data flow

1. `albert up` → agent starts → inventory scans all providers in parallel → cache written → UI loads `/api/tools`.
2. Background outdated check → inventory updated → SSE event → UI updates badges.
3. User installs a tool elsewhere → fsnotify → debounce → rescan that provider → tool marked `New` → SSE → UI shows it.
4. User clicks Update → `POST /api/update` → job → output streamed over SSE → on finish, provider rescanned → UI updated.

## 5. Error handling

- Provider not installed: `Available()` false, provider hidden from counts, listed in `albert doctor`.
- Provider command fails/times out: provider marked unavailable with the error; other providers unaffected.
- Parse error: same as failure, raw output logged to `~/.albert/albert.log`.
- Agent crash: `albert status` reports it; `albert down` cleans up; `albert up` restarts cleanly.
- Docker missing/stopped: native fallback offered.

## 6. Testing

- Provider parsers: unit tests against captured real-output fixtures in `testdata/`, using a fake runner. No real installs or upgrades in tests.
- Inventory: merge, `New` detection, timeout isolation (a hanging fake provider does not block others).
- Watcher: temp dirs, debounce behaviour.
- Agent: `httptest` for every endpoint; token auth rejects missing/wrong tokens; requests with a foreign `Host`/`Origin` or a `POST` without `X-Albert-Request` are rejected; SSE emits events.
- Jobs: per-provider serialization, sudo-failure detection.
- Manual smoke test on a real Mac: `albert up`, all tools visible, install a small tool in Terminal and see it appear, update one harmless tool, `albert down`.

## 7. Repository layout

```
cmd/albert/main.go
internal/runner/
internal/provider/        brew.go cask.go uv.go pipx.go pip.go npm.go pnpm.go bun.go mas.go cargo.go gotools.go gem.go
internal/inventory/
internal/watch/
internal/jobs/
internal/agent/
internal/dock/
internal/state/
web/                      index.html, js/*.js, styles.css, embed.go
docker/                   Dockerfile, Caddyfile
.github/workflows/        test.yml, release.yml (goreleaser), image.yml (GHCR)
install.sh
README.md
```

## 8. Distribution

- GitHub Releases with darwin arm64 + amd64 binaries via goreleaser.
- `install.sh` one-liner: `curl -fsSL https://raw.githubusercontent.com/kamllistos/albert/main/install.sh | sh` (installs to `~/.local/bin` or `/usr/local/bin`).
- GUI image published to `ghcr.io/kamllistos/albert-ui` by GitHub Actions on tag; local build fallback until then.
- Homebrew tap deferred.
