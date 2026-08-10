---
name: gdm-maintainer
description: Maintain GDM (Go Download Manager), a Go TUI for segmented HTTP and BitTorrent downloads. Use for code changes, debugging, tests, documentation, and release work in this repository.
argument-hint: "[maintenance task]"
---

# GDM Maintainer

Use this skill for work inside this repository. Keep changes small, boring, and testable.

## Project facts

- Module: `github.com/wongpinter/gdm`
- Binary: `cmd/gdm`
- Runtime state stays at `~/.idm/state` for compatibility. Do not rename it without an explicit migration plan.
- Downloads default to `~/Downloads`.
- No new dependency unless existing standard-library or installed code cannot solve the problem.
- HTTP and torrent downloads share `domain.Download`; manager owns lifecycle, persistence, and concurrency.

## Architecture

- `internal/domain`: download and segment entities; no I/O.
- `internal/manager`: application service; queue, worker limits, persistence, pause/resume, snapshots, and ports.
- `internal/engine`: HTTP probe and ranged-download adapter.
- `internal/torrentengine`: shared `anacrolix/torrent.Client` adapter.
- `internal/store`: JSON state adapter.
- `internal/tui`: Bubble Tea model, update loop, rendering, and formatting.
- `cmd/gdm`: composition root.

Manager depends on interfaces in `internal/manager/ports.go`, not concrete adapters.

## Torrent rules

- `TorrentEngine.Start(ctx, id, download, stats)` creates or reuses a torrent keyed by manager ID.
- Pause cancels the manager context; engine calls `DisallowDataDownload()` but retains torrent state.
- Resume reuses the retained torrent and calls `AllowDataDownload()`.
- Only explicit `Remove(id)` calls `Drop()` and forgets the torrent.
- `Close()` shuts down the shared torrent client.
- Preserve these semantics in code, tests, and documentation. Do not describe pause/resume as remove-and-readd unless behavior changes intentionally.

## Change workflow

1. Inspect the nearest public seam and its callers before editing.
2. Add or update one focused test for non-trivial behavior. For bug fixes, reproduce the bug first.
3. Make the smallest implementation that passes the test. Avoid new abstractions and unrelated refactors.
4. Run formatting and verification before reporting completion.
5. Update README or comments when public behavior changes.

## Verification

Run from repository root:

```bash
gofmt -d cmd/gdm/main.go internal/**/*.go
go test -race -timeout 120s ./...
go vet ./...
```

Use `go test ./internal/tui` for focused TUI work. There is no task runner; do not create one for a one-off change.

## TUI rules

Preserve existing controls unless requested otherwise:

- `a` add URL, magnet, or `.torrent`
- `p` pause
- `r` resume
- `x` remove and delete file
- arrows, `j`, `k` select
- `q` quit

Check empty queues, failed downloads, long names/URLs, unknown torrent metadata, narrow widths, and short terminal heights when changing rendering or input handling.

## Release guardrails

- Keep user-facing branding as GDM / Go Download Manager.
- Keep module/import paths under `github.com/wongpinter/gdm`.
- Do not commit secrets, generated binaries, local state, or credentials.
- Prefer one focused commit using Conventional Commits.
- Confirm branch and remote before pushing; default branch is `main`.
