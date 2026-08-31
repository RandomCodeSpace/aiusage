<div align="center">

# aiusage

**One local dashboard for the tokens, cost, and activity from your AI coding tools.**

[![Release](https://img.shields.io/github/v/release/RandomCodeSpace/aiusage?style=for-the-badge&logo=github&logoColor=white&label=release)](https://github.com/RandomCodeSpace/aiusage/releases)
[![CI](https://img.shields.io/github/actions/workflow/status/RandomCodeSpace/aiusage/ci.yml?branch=main&style=for-the-badge&logo=githubactions&logoColor=white&label=ci)](https://github.com/RandomCodeSpace/aiusage/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/GO-1.25.13-00ADD8?style=for-the-badge&logo=go&logoColor=white)](https://pkg.go.dev/github.com/RandomCodeSpace/aiusage)
[![Platforms](https://img.shields.io/badge/LINUX_%7C_MACOS-111111?style=for-the-badge&logo=linux&logoColor=white)](https://github.com/RandomCodeSpace/aiusage/releases)
[![License](https://img.shields.io/github/license/RandomCodeSpace/aiusage?style=for-the-badge&color=2ea44f)](./LICENSE)

</div>

<p align="center">
  <img src="./assets/dashboard.png" alt="aiusage terminal dashboard showing token use, cost, trends, and usage by tool" width="1280">
</p>

<p align="center"><sub>Real aiusage dashboard, captured with synthetic demo data.</sub></p>

## What it does

`aiusage` finds the usage records your coding agents already keep and turns
them into one searchable history.

- See tokens and cost for today, this week, or any date range.
- Compare tools, models, providers, projects, and sessions.
- Explore tool calls, file changes, commands, searches, and other agent activity.
- Keep historical totals even after an agent compacts or removes a transcript.
- Use the terminal dashboard, plain-text reports, JSON, CSV, or the Go packages.

Your source files are read-only. Running collection again is safe: observations
that were already recorded are not counted twice.

## Install

Download a ready-to-run archive for Linux or macOS from
[GitHub Releases](https://github.com/RandomCodeSpace/aiusage/releases).

If you have Go 1.25.13 or newer:

~~~console
go install github.com/RandomCodeSpace/aiusage@latest
~~~

Release builds support Linux and macOS on amd64 and arm64. Windows is not
currently supported.

## First run

Check which tools and data sources are available:

~~~console
aiusage doctor
~~~

Import everything that is already on this machine:

~~~console
aiusage once
~~~

Then choose a view:

~~~console
aiusage today    # quick report for today
aiusage          # interactive dashboard
~~~

`doctor` explains missing or unreadable sources and prints setup help when a
tool needs it. GitHub Copilot, for example, needs its OpenTelemetry file
exporter enabled before token data is available.

## Supported tools

Antigravity · Claude Code · Cline · Codex · Crush · DSH · GitHub Copilot ·
Goose · Hermes · Kimi Code · OpenClaw · opencode · Pi · Qwen Code · Reasonix

Coverage varies because each tool records different information. Some provide
exact cost and detailed activity; others provide token totals only. Run
`aiusage doctor` to see what is available on your machine.

## Reading the numbers

Cost markers keep incomplete data honest:

| Marker | Meaning |
|---|---|
| `$` | Cost reported directly by the tool |
| `~$` | Cost estimated from the model price table |
| `≥` | Some usage could not be priced, so the total is a minimum |
| `-` | No known cost is available |

Activity dimensions overlap. A single agent turn can read a file, run a
command, and call another tool, so activity columns should not be added
together as one total.

## Everyday commands

| Command | What it gives you |
|---|---|
| `aiusage` | Interactive terminal dashboard |
| `aiusage today` | Today's totals |
| `aiusage last 2d` | A rolling time window |
| `aiusage summary` | Filtered and grouped reports |
| `aiusage once` | One collection pass, then exit |
| `aiusage sources` | Discovered source files and their status |
| `aiusage doctor` | Setup and health checks |
| `aiusage export` | JSON or CSV export |
| `aiusage setup` | Install background collection |
| `aiusage version` | Build and schema version |

Examples:

~~~console
aiusage last 2d
aiusage summary --since 7d --by day,tool,model --breakdown
aiusage summary --since 2026-08-01 --until 2026-09-01 --by provider --json
aiusage export --since 30d --format csv --out usage.csv
~~~

Run `aiusage <command> --help` for every option.

## Dashboard controls

| Key | Action |
|---|---|
| `1` to `5` | Switch between Overview, Tools, Models, Sessions, and Activity |
| `Tab` | Move to the next view |
| `t` | Change the time range |
| `[` / `]` | Move the time window backward or forward |
| `p` | Change provider scope |
| `/` | Search or filter the current view |
| `s` | Change sorting |
| `Enter` / `Esc` | Open or close details |
| `r` | Refresh |
| `q` | Quit |

The footer always shows the controls available in the current view.

## Keep collection running

Opening the dashboard or a report makes sure a collector is running. The
default interval is five minutes.

On Linux with systemd, `aiusage setup` installs a user service without
`sudo`. On macOS, and on Linux without systemd, aiusage starts a detached
collector; start it again after a reboot.

For manual control:

~~~console
aiusage run             # foreground collector
aiusage once            # collect once
aiusage --no-daemon     # report without starting a collector
~~~

Only one collector writes to a database at a time.

## Configuration

No configuration file is required. When present, it lives at
`~/.config/aiusage/config.json` (or the matching XDG config directory).

A small personal setup might look like this:

~~~json
{
  "interval_seconds": 300,
  "privacy": {
    "no_raw": true
  },
  "pricing": {
    "refresh": false
  }
}
~~~

Common overrides:

- `AIUSAGE_HOME` changes the aiusage data directory.
- `AIUSAGE_DB` changes the database path.
- `AIUSAGE_INTERVAL` changes the collector interval.
- `--db`, `--config`, and `--interval` override one command.
- `source_roots` in the config file adds non-standard source locations.

The default database is `~/.local/share/aiusage/usage.db`. Run
`aiusage doctor` to print the exact paths in use.

## Privacy and network access

- aiusage only reads the coding-tool records it discovers.
- It writes its own database, config, lock, cache, and log files.
- Raw metadata is limited to an allowlist of supported fields.
- `privacy.no_raw` prevents new raw metadata from being stored; it does not
  erase metadata already in the database.
- Normal reports and exports do not include raw metadata.

Token collection itself is local. When pricing refresh is enabled, aiusage may
download the LiteLLM price table and cache a successful response for 24 hours.
If refresh fails, the bundled table remains in use and a later pass may retry.

<details>
<summary><strong>Use aiusage from Go</strong></summary>

`aiusage` also exposes the components used by the CLI:

| Package | Purpose |
|---|---|
| `usage` | Stable event, query, and pricing types |
| `adapter` | Adapter interfaces and source descriptors |
| `adapter/all` | Built-in adapter registry |
| `collector` | One-shot and continuous collection |
| `store` | SQLite storage and query API |
| `pricing` | Bundled and refreshable model prices |
| `config` | Configuration loading |

Open an existing database with `store.OpenReadOnly`. Use `store.Open` only
for collection or migrations, then call `collector.RunOnce` or
`collector.Run`.

API documentation is available on
[pkg.go.dev](https://pkg.go.dev/github.com/RandomCodeSpace/aiusage).

</details>

<details>
<summary><strong>Build and test</strong></summary>

~~~console
go build ./...
go test ./...
go vet ./...
gofmt -l .
~~~

Release binaries are built with CGO disabled.

</details>

## Versioning

The project uses semantic version tags. Database migrations are automatic and
forward-only, so back up the database before opening it with an older build.
While aiusage is at v0.x, breaking Go API changes are limited to minor
releases; patch releases preserve consumer compatibility.

## License

[MIT](./LICENSE)
