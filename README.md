# gdm

GDM (Go Download Manager) is a small download manager in Go: multi-connection
segmented HTTP downloads, BitTorrent downloads, pause/resume, crash-safe
persistence, and a terminal dashboard — no GUI, no daemon, just one binary.

## Features

- **Segmented HTTP downloads** — splits a file across N connections
  (`Range` requests) and writes each segment straight into its slot in
  the destination file via `WriteAt`, so there's no separate merge step.
- **BitTorrent downloads** — magnet URIs or local `.torrent` files,
  backed by `anacrolix/torrent`. Detected automatically (a `magnet:`
  prefix or `.torrent` extension) wherever you add a download — CLI
  args or the in-dashboard "add" box.
- **Pause / resume** — for HTTP, pausing cancels the in-flight request
  context and persists exactly how many bytes each segment has,
  resuming re-issues `Range: bytes=<offset>-<end>` from there. For
  torrents, pausing drops the swarm connection but leaves whatever
  pieces have already landed on disk; resuming re-adds the torrent and
  the library re-verifies what's already there.
- **Crash recovery** — every download's state lives in its own JSON file
  (atomic write via temp file + rename). On restart, anything that was
  `downloading` when the process died comes back as `paused`, never
  silently resumed behind your back.
- **Fallback for non-range HTTP servers** — if a server doesn't
  advertise `Accept-Ranges: bytes`, the download drops to a single
  connection instead of failing outright.
- **TUI dashboard** — live progress bars, speed, ETA, peer counts for
  torrents, add/pause/resume/remove, all from the terminal.

## Usage

```sh
go build -o gdm ./cmd/gdm

./gdm                                     # open the empty dashboard
./gdm https://example.com/big-file.iso    # queue an HTTP download
./gdm 'magnet:?xt=urn:btih:...'           # queue a torrent by magnet URI
./gdm ./linux-distro.torrent              # queue a torrent from a local file
```

Flags:

| Flag           | Default            | Meaning                                   |
|----------------|---------------------|--------------------------------------------|
| `-dir`         | `~/Downloads`        | where finished files are written           |
| `-state`       | `~/.idm/state`       | where per-download JSON state is kept      |
| `-connections` | `4`                  | default segments per HTTP download         |
| `-max-active`  | `3`                  | how many downloads run at once             |

Keys inside the dashboard: `a` add a URL, magnet link, or `.torrent`
path · `p` pause · `r` resume · `x` remove (and delete the file) ·
`↑/↓` select · `q` quit.

If the torrent client can't start (e.g. a container with no IPv6 stack
at all — it retries once with IPv6 disabled before giving up), torrent
support is simply skipped and HTTP downloads keep working; a message
is printed to stderr explaining why.

## Architecture

Hexagonal, with the domain at the center and everything else an
interchangeable adapter:

```
internal/domain      — Download / Segment entities, no I/O. Kind
                        ("http"/"torrent") picks which run path a
                        download takes.
internal/manager      — application service: queue, concurrency, pause/
                        resume, progress aggregation. Declares the
                        ports it needs (Store, Engine, TorrentEngine)
                        as consumer-defined interfaces in ports.go.
internal/engine       — outbound adapter: net/http implementation of
                        the Engine port (probing + ranged downloads)
internal/torrentengine — outbound adapter: anacrolix/torrent
                        implementation of the TorrentEngine port
internal/store        — outbound adapter: JSON-file implementation of
                        the Store port
internal/tui          — inbound adapter: bubbletea dashboard that
                        drives the manager
cmd/gdm               — composition root: wires adapters into the
                        manager and starts the TUI
```

`internal/manager` never imports `internal/engine`, `internal/torrentengine`,
or `internal/store` — it only knows about the interfaces it declared for
itself. Swapping the JSON store for SQLite, or adding a third download
kind entirely, means writing a new adapter, not touching the manager.

HTTP and torrent downloads share the domain model despite very different
engines: a torrent is represented as one pseudo-`Segment` spanning the
whole transfer (`{Start:0, End:TotalSize-1, Downloaded:BytesCompleted}`),
so `Download.BytesDownloaded()`/`Progress()` work unchanged for both —
the manager runs a `runTorrentDownload` loop parallel to `runDownload`,
consuming periodic `TorrentStats` snapshots from `TorrentEngine.Start` instead of aggregating
per-chunk `ProgressEvent`s, but everything downstream (persistence, the
TUI, pause/resume semantics) is the same code path.

### Concurrency model

Each download runs in its own goroutine (bounded by `-max-active` via a
semaphore). Within a download, each unfinished segment gets its own
goroutine performing a ranged GET and writing into the shared file at
its own offset — safe without a lock because segments never overlap.
Progress flows back as a stream of `{segment, bytesWritten}` deltas on
a channel; the owning goroutine is the only writer of that download's
mutable state, so reads from the TUI (via `Manager.List`/`Get`) take a
per-download lock and hand back a deep copy.

### Tests

`internal/manager/manager_test.go` runs the real HTTP engine and store
against a local `httptest.Server` (using `http.ServeContent`, so real
`Range` handling) — a full download with checksum verification, a
pause-mid-transfer-then-resume round trip, and a simulated process
restart that confirms an interrupted download comes back `paused`
rather than resuming itself.

`internal/torrentengine/torrentengine_test.go` runs a real, fully
offline BitTorrent transfer: two in-process `torrent.Client`s (a seeder
and a leecher) peered directly via `AddClientPeer`, no DHT or trackers
involved, checksummed end to end.

```sh
go test ./... -race
```

### A note on `go.mod`

`anacrolix/torrent` pulls in a long tail of dependencies that live at
vanity import domains (`golang.org/x/*`, `gopkg.in/*`,
`go.opentelemetry.io/*`, and a few more) rather than directly on
GitHub. The `replace` block in `go.mod` redirects each of those to its
official GitHub mirror at a matching commit or tag. A handful of
entries point at a small local stub under `third_party/` instead —
these are old test/lint tooling (`gopkg.in/errgo.v2`, `golang.org/x/lint`,
`honnef.co/go/tools`, `cloud.google.com/go`, `gopkg.in/alecthomas/kingpin.v2`)
pulled in only because some very old transitive dependency predates
Go's module graph pruning; nothing in this project's actual build
imports them, so an empty stub package satisfies the module graph
without needing the real (unused) code. If your environment has normal
access to the Go module proxy, none of this is necessary — the
`replace` block and `third_party/` directory can both be deleted, and
`go mod tidy` will resolve everything the standard way.

## Known limitations

- No bandwidth throttling or recurring time-window scheduling; one-time
  "download later" scheduling is available through the manager API.
- No browser integration / clipboard URL monitoring.
- HTTP segment count is fixed per download at creation time — it
  doesn't dynamically add connections to a slow segment.
- Paused torrents retain their swarm and piece state in the shared client;
  resume reuses that retained torrent. Explicit removal drops the torrent.
- No DHT/tracker configuration exposed (uses the library's defaults);
  no seeding-after-complete toggle.
