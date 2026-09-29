# gdm

GDM (Go Download Manager) is a single-binary Go download manager for terminal and headless environments. It supports segmented HTTP downloads, BitTorrent magnets and `.torrent` files, pause/resume, crash-safe state, and media organization.

## Requirements

- Go 1.24.4 or newer
- Writable download and state directories
- Network access for HTTP downloads and torrent metadata/peers
- TMDb credentials only for movie organization
- TV organization uses TVMaze

## Build and run

```sh
go build -o gdm ./cmd/gdm

./gdm                                      # terminal dashboard
./gdm https://example.com/file.iso         # queue HTTP download
./gdm 'magnet:?xt=urn:btih:...'             # queue torrent
./gdm ./linux-distro.torrent               # queue local torrent
./gdm --headless                            # process unfinished queue
```

Flags can appear before or after URLs:

| Flag | Default | Purpose |
|---|---|---|
| `-dir` | `~/Downloads` | Finished-file directory |
| `-state` | `~/.gdm/state` | Per-download JSON state |
| `-connections` | `4` | HTTP connections per download |
| `-max-active` | `3` | Concurrent active downloads |
| `-interval` | `2s` | Headless progress refresh |
| `-public-trackers` | `false` | Add fallback trackers before magnet metadata; reveals info hash |
| `--headless` | `false` | Run without TUI and return non-zero on failure |

Headless examples:

```sh
./gdm --headless -dir /data/downloads 'magnet:?xt=urn:btih:...'
./gdm --headless -interval 500ms
```

No TTY automatically selects headless mode. A completed torrent is not accepted from piece state alone: gdm checks every final file, expected size, and piece hash before returning success. Full `.part` data is locally rehashed and promoted when possible; corrupt or incomplete data returns to the normal swarm path.

## Dashboard keys

`a` add URL, magnet, or `.torrent` path · `p` pause · `r` resume · `x` remove and delete downloaded data · `↑/↓` select · `q` quit.

## Media organizer

Organizer commands default to dry-run. Add `--apply` only after reviewing planned paths. Existing files and collisions are not overwritten.

```sh
# Preview TV organization
./gdm organize tv --input /downloads --output /library/TV --query "The Simpsons"

# Apply movie organization
TMDB_API_KEY=YOUR_KEY ./gdm organize movie \
  --input "/downloads/Arrival (2016).mkv" \
  --output /library/Movies --apply
```

TV matching reads `SxxEyy` and can use TVMaze. Movie matching uses TMDb and requires `TMDB_API_KEY` or `TMDB_READ_ACCESS_TOKEN`. Supported media extensions: `mkv`, `mp4`, `m4v`, `avi`, `mov`, `mpeg`, `mpg`, `ts`, `webm`, `wmv`.

## State and recovery

State is stored as one JSON file per download under `~/.gdm/state`. Writes use a temporary file and atomic rename. Interrupted active downloads load as `paused`; gdm does not silently spend bandwidth after restart.

The old `~/.idm/state` directory migrates to `~/.gdm/state` once. Override with `-state` when using a custom location. Torrent metainfo cache lives beside state at `~/.gdm/torrents` by default.

Torrent recovery rules:

- Final files are authoritative only after size and piece-hash verification.
- A completed torrent with leftover `.part` data loads as `paused`.
- Resuming a full `.part` file hashes local data first and avoids re-download.
- A promotion failure is surfaced as an error, not reported as completed.
- Zero-byte files are invalid for non-empty torrent files and are rechecked by size/hash.

If a queue entry says completed but disk data is missing, stop gdm, verify `-dir` matches the original destination, rebuild the binary, and restart:

```sh
go build -o gdm ./cmd/gdm
./gdm --headless
```

## Architecture

The repository uses ports-and-adapters boundaries:

```text
cmd/gdm                 composition root, CLI, headless mode
internal/domain         Download and Segment entities; no I/O
internal/manager        queue, lifecycle, concurrency, progress, ports
internal/engine         segmented HTTP adapter
internal/torrentengine  anacrolix/torrent adapter and disk verification
internal/store          atomic JSON persistence adapter
internal/mediaorg       TV/movie discovery and safe copy planning
internal/tui             Bubble Tea dashboard adapter
third_party             narrow module stubs for legacy transitive tooling
skills                  maintainer workflow notes
```

`internal/manager` depends on interfaces declared in `internal/manager/ports.go`, not concrete transport or persistence adapters. Keep new integrations behind those ports. Keep filesystem and network validation at adapter boundaries. Do not bypass manager lifecycle methods by mutating stored JSON manually while gdm is running.

Concurrency rules:

- Manager owns lifecycle transitions and limits active downloads with a semaphore.
- Each download worker owns its mutable download state.
- `Manager.List` and `Manager.Get` return copies for readers such as TUI.
- HTTP segments write disjoint ranges with `WriteAt`.
- Store writes and deletes are serialized and reject unsafe IDs.

## Development

Run focused checks during changes:

```sh
gofmt -w ./cmd ./internal
go test ./...
go test -race ./...
go vet ./...
golangci-lint run ./...
```

Run one package while iterating:

```sh
go test ./internal/torrentengine -run 'TestStart|TestEndToEnd' -count=1 -v
go test ./internal/manager -run 'Test.*' -count=1 -v
```

Tests use local `httptest` servers and offline in-process torrent seeders. They do not require public trackers. Avoid tests that depend on real external media APIs or network timing.

Before submitting changes:

1. Keep changes inside the relevant package boundary.
2. Add a focused regression test for non-trivial behavior.
3. Run formatting, unit tests, race tests, vet, and lint.
4. Check `git diff` for generated binaries, credentials, state files, and accidental path changes.

## Known limitations

- No bandwidth throttling or recurring schedule.
- No browser integration or clipboard monitoring.
- HTTP segment count is fixed when a download is created.
- Paused torrents retain state in the current client; explicit removal drops the torrent.
- No seeding-after-complete toggle.
- Public fallback trackers are opt-in before metadata and disclose the torrent info hash.

## License

See [LICENSE](LICENSE).
