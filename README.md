# Albert

Albert shows every tool installed on your Mac by Homebrew, pip, npm and other package managers in one place, tells you which ones have updates, and lets you update or install them with a click. It runs on your Mac; the only network traffic is your package managers and version lookups on public registries.

## Install

Paste this into Terminal:

```sh
curl -fsSL https://raw.githubusercontent.com/kamllistos/albert/main/install.sh | sh
```

Albert works on Apple silicon and Intel Macs. It installs into `~/.local/bin`, and the installer tells you if that folder needs adding to your PATH.

## Use it

Two commands are all you need:

```sh
albert up      # open Albert in your browser
albert down    # stop Albert
```

`albert up` opens Albert in your browser. It keeps running in the background, even if you close Terminal, until you run `albert down`.

Docker (Docker Desktop or OrbStack) is optional. If it's running, Albert uses it for its window. If it isn't, Albert asks whether to open without it, and opens in your browser directly. Everything works the same either way.

Some updates need your Mac password (for example, tools installed system-wide). Albert can't ask for it in the browser, so it tells you to run `albert update <tool>` in Terminal instead.

## Command line

You can also use Albert without the browser.

| Command | What it does |
| --- | --- |
| `albert up [--native] [--no-open]` | Start Albert and open it in your browser. `--native` skips Docker. `--no-open` doesn't open the browser. |
| `albert down` | Stop Albert. |
| `albert status` | Show whether Albert is running, and where. |
| `albert list [--source S] [--outdated] [--deps] [--json]` | Show installed tools. `--source` limits to one source, `--outdated` checks for updates and shows only tools that have one, `--deps` includes tools installed as dependencies of others, `--json` prints machine-readable output. |
| `albert outdated [--source S]` | Show only the tools that have a newer version. |
| `albert update <tool> [--source S]` | Update one tool. Use `--source` if several sources have a tool with that name. |
| `albert update --all [--source S] [--include-pip]` | Update every tool that has an update. pip is skipped unless you add `--include-pip`. |
| `albert install <tool> [--source S]` | Install a tool. Without `--source`, Albert searches all sources and asks if more than one has it. |
| `albert search <query>` | Search all sources for a tool to install. |
| `albert doctor` | Check which package managers (and Docker) Albert can use. |
| `albert version` | Show Albert's version. |
| `albert help` | Show the list of commands. |

## Supported sources

| Source | What it covers | Notes |
| --- | --- | --- |
| `brew` | Homebrew formulae (command-line tools) | |
| `cask` | Homebrew casks (Mac apps) | |
| `uv` | Tools installed with `uv tool install` | |
| `pipx` | Tools installed with pipx | |
| `pip` | Python packages | Read-only unless installed with `--user`. "Update all" skips pip unless you use `--include-pip`. |
| `npm` | Global npm packages | |
| `pnpm` | Global pnpm packages | |
| `bun` | Global bun packages | |
| `mas` | Mac App Store apps | Install by numeric App Store ID. |
| `cargo` | Rust tools installed with `cargo install` | Tools installed from a path or git repository are left alone. |
| `go` | Go tools installed with `go install` | |
| `gem` | Ruby gems | Gems in the macOS system Ruby are read-only. |

"Read-only" tools are shown but Albert won't change them, because something else manages them. Sources whose package manager isn't installed are simply skipped; `albert doctor` shows which ones Albert found.

## How it works

Albert has two parts:

- **The agent** is a small background program (the `albert` binary itself, started by `albert up`). It runs your package managers, remembers results, watches for changes and does the installs and updates. It only listens on `127.0.0.1`, so it can't be reached from other computers.
- **The window** is the web page you see. With Docker it is served from a small container named `albert-ui` (a Caddy web server that only serves the page and passes requests to the agent). Without Docker, the agent serves the same page itself.

Albert keeps its files in `~/.albert` (override with `ALBERT_HOME`): `state.json`, `cache.json`, `token`, and `albert.log`, which is where to look if something goes wrong.

## Security

- Albert only listens on localhost (`127.0.0.1`).
- The window container holds no secret. A random access token is created each time you run `albert up` and kept in a private file (`~/.albert/token`, readable only by you).
- `albert up` opens a one-time sign-in link in your browser. The link works once and expires after 60 seconds. It sets a cookie that is HttpOnly (page scripts can't read it) and SameSite=Strict, and the browser sends it back to Albert.
- The agent checks every request: the token, the `Host` and `Origin` headers, `Sec-Fetch-Site`, and a custom `X-Albert-Request` header on anything that changes things. It sends no CORS headers, so other websites can't talk to it.
- The session ends at `albert down` (or the next `albert up`); an old link or cookie then stops working. If the session ends while Albert is open, the page tells you to run `albert up` again.

Known limitation: browsers don't separate cookies by port. The Albert session cookie is also sent to any other service on `localhost` that you visit in the same browser (for example, a development server on another port). It is only valid until `albert down` or the next `albert up`, so run `albert down` when you aren't using Albert.

## Development

```sh
go test ./...          # run the tests
go vet ./...
go build ./cmd/albert  # build the binary
```

Albert needs Go (the version in `go.mod`) and only depends on `fsnotify`. The tests never run real installs and never touch the network.

The web page lives in `web/` and the container recipe in `docker/`; both are embedded in the binary. A `dev` build builds the container image locally the first time you run `albert up`; release builds pull `ghcr.io/kamllistos/albert-ui:<version>` and fall back to building locally if that fails.

### Releasing (maintainers)

Pushing a tag like `v0.1.0` runs two workflows: `release.yml` (GoReleaser builds `albert_darwin_arm64.tar.gz`, `albert_darwin_amd64.tar.gz` and `checksums.txt` for a GitHub release; `install.sh` downloads these names, so change them together) and `image.yml` (builds the `albert-ui` image for linux/amd64 and linux/arm64 and pushes it to GHCR as `:<version>` and `:latest`). `test.yml` runs `go vet` and `go test -race` on every push and pull request.

Note: the repository `kamllistos/albert` is currently private. Until it is public, the `curl` installer above and the GHCR image can't be reached by other people. When making it public, also check that the `albert-ui` package on GHCR is set to public.
