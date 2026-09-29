package torrentengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/wongpinter/gdm/internal/domain"
	"github.com/wongpinter/gdm/internal/manager"
)

type startResult struct {
	first     manager.TorrentStats
	haveFirst bool
	doneSeen  bool
	err       error
}

// setupSeed builds a single-file torrent of size bytes and a seeding
// client, returning the .torrent path, its info hash, and the seeder.
func setupSeed(t *testing.T, size int) (string, metainfo.Hash, *torrent.Client) {
	t.Helper()
	seedDir := t.TempDir()
	_, mi, infoHash := buildTorrent(t, seedDir, "seed.bin", size)
	seed, err := torrent.NewClient(offlineConfig(seedDir))
	if err != nil {
		t.Fatalf("seed client: %v", err)
	}
	t.Cleanup(func() { _ = seed.Close() })
	st, err := seed.AddTorrent(&mi)
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	<-st.GotInfo()
	st.DownloadAll()

	torrentPath := filepath.Join(t.TempDir(), "seed.torrent")
	f, err := os.Create(torrentPath)
	if err != nil {
		t.Fatalf("create .torrent: %v", err)
	}
	if err := mi.Write(f); err != nil {
		t.Fatalf("write .torrent: %v", err)
	}
	_ = f.Close()
	return torrentPath, infoHash, seed
}

// newLeech builds an Engine over dataDir with its own client — a stand-in
// for a new process. The caller owns closing eng.client before opening
// another client on the same directory.
func newLeech(t *testing.T, dataDir string) *Engine {
	t.Helper()
	lc, err := torrent.NewClient(offlineConfig(dataDir))
	if err != nil {
		t.Fatalf("leech client: %v", err)
	}
	t.Cleanup(func() { _ = lc.Close() })
	return &Engine{client: lc, dataDir: dataDir}
}

// startAndWait drives one eng.Start to a terminal return.
func startAndWait(t *testing.T, eng *Engine, seed *torrent.Client, infoHash metainfo.Hash, torrentPath string, wirePeer bool, timeout time.Duration) startResult {
	t.Helper()
	eng.SetMetainfoCache(t.TempDir())
	dl := &domain.Download{URL: torrentPath, Kind: domain.KindTorrent}
	stats := make(chan manager.TorrentStats, 64)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- eng.Start(ctx, "probe", dl, stats) }()
	if wirePeer {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if lt, ok := eng.client.Torrent(infoHash); ok {
				lt.AddClientPeer(seed)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	var res startResult
	for {
		select {
		case st := <-stats:
			if !res.haveFirst {
				res.first, res.haveFirst = st, true
			}
			if st.Done {
				res.doneSeen = true
			}
		case err := <-errCh:
			res.err = err
			for {
				select {
				case st := <-stats:
					if !res.haveFirst {
						res.first, res.haveFirst = st, true
					}
					if st.Done {
						res.doneSeen = true
					}
					continue
				default:
				}
				break
			}
			return res
		case <-ctx.Done():
			res.err = ctx.Err()
			return res
		}
	}
}

func assertFinalized(t *testing.T, final string) {
	t.Helper()
	fi, err := os.Stat(final)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("final file %s not promoted: %v", final, err)
	}
	if _, err := os.Stat(final + ".part"); !os.IsNotExist(err) {
		t.Fatalf(".part still present after repair")
	}
}

// TestStartPromotesLeftoverPartFileWithoutPeers pins the reported bug:
// resume said finished while the payload still sat in .part. After a
// restart with no peers wired, the engine must rehash locally and
// promote the file instead of stalling or lying about completion.
func TestStartPromotesLeftoverPartFileWithoutPeers(t *testing.T) {
	torrentPath, infoHash, seed := setupSeed(t, 300_000)
	leechDir := t.TempDir()
	final := filepath.Join(leechDir, "seed.bin")

	// Normal download first, so the .part below carries a real payload.
	eng1 := newLeech(t, leechDir)
	res := startAndWait(t, eng1, seed, infoHash, torrentPath, true, 30*time.Second)
	if res.err != nil || !res.doneSeen {
		t.Fatalf("initial download: done=%v err=%v", res.doneSeen, res.err)
	}
	if err := os.Rename(final, final+".part"); err != nil {
		t.Fatalf("renaming to .part: %v", err)
	}
	if err := eng1.client.Close(); err != nil {
		t.Fatalf("closing first client: %v", err)
	}

	// Fresh client (new process), no peers: must heal from disk alone.
	eng2 := newLeech(t, leechDir)
	res = startAndWait(t, eng2, seed, infoHash, torrentPath, false, 30*time.Second)
	if res.err != nil {
		t.Fatalf("resume without peers: %v", res.err)
	}
	if !res.doneSeen {
		t.Fatal("no done snapshot after repair")
	}
	assertFinalized(t, final)
}

// TestStartRepairsUnfinalizedDataInProcess covers the pause/resume
// window: the client still holds the completed torrent, so resume used
// to return done immediately with .part left on disk.
func TestStartRepairsUnfinalizedDataInProcess(t *testing.T) {
	torrentPath, infoHash, seed := setupSeed(t, 300_000)
	leechDir := t.TempDir()
	final := filepath.Join(leechDir, "seed.bin")

	eng := newLeech(t, leechDir)
	res := startAndWait(t, eng, seed, infoHash, torrentPath, true, 30*time.Second)
	if res.err != nil || !res.doneSeen {
		t.Fatalf("initial download: done=%v err=%v", res.doneSeen, res.err)
	}
	if err := os.Rename(final, final+".part"); err != nil {
		t.Fatalf("renaming to .part: %v", err)
	}

	res = startAndWait(t, eng, seed, infoHash, torrentPath, false, 15*time.Second)
	if res.err != nil {
		t.Fatalf("in-process resume: %v", res.err)
	}
	assertFinalized(t, final)
}

// TestStartFailsWhenPromotionIsBlocked: with a directory squatting on
// the final path the .part can never be renamed over it — Start must
// fail loudly naming the file instead of reporting the download done.
func TestStartFailsWhenPromotionIsBlocked(t *testing.T) {
	torrentPath, infoHash, seed := setupSeed(t, 300_000)
	leechDir := t.TempDir()
	final := filepath.Join(leechDir, "seed.bin")

	eng1 := newLeech(t, leechDir)
	res := startAndWait(t, eng1, seed, infoHash, torrentPath, true, 30*time.Second)
	if res.err != nil || !res.doneSeen {
		t.Fatalf("initial download: done=%v err=%v", res.doneSeen, res.err)
	}
	if err := os.Rename(final, final+".part"); err != nil {
		t.Fatalf("renaming to .part: %v", err)
	}
	if err := os.Mkdir(final, 0o755); err != nil {
		t.Fatalf("blocking final path: %v", err)
	}
	if err := eng1.client.Close(); err != nil {
		t.Fatalf("closing first client: %v", err)
	}

	eng2 := newLeech(t, leechDir)
	res = startAndWait(t, eng2, seed, infoHash, torrentPath, false, 15*time.Second)
	if res.err == nil {
		t.Fatal("want error for blocked promotion, got nil")
	}
	if !strings.Contains(res.err.Error(), "not finalized") {
		t.Fatalf("error should name the finalize failure, got: %v", res.err)
	}
	if _, err := os.Stat(final + ".part"); err != nil {
		t.Fatalf(".part should remain after the failed rename: %v", err)
	}
}
