package torrentengine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/wongpinter/gdm/internal/domain"
	"github.com/wongpinter/gdm/internal/manager"
)

// offlineConfig returns a ClientConfig with no DHT, trackers, or UPnP —
// this test peers two local clients directly via AddClientPeer, so it
// needs no network access beyond localhost.
func offlineConfig(dataDir string) *torrent.ClientConfig {
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = dataDir
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.NoDefaultPortForwarding = true
	cfg.ListenPort = 0
	cfg.Seed = true
	cfg.DisableIPv6 = true
	return cfg
}

// buildTorrent writes size random bytes to <seedDir>/<name>, builds its
// metainfo, and returns the parsed Info, the encoded MetaInfo, and its
// info hash.
func buildTorrent(t *testing.T, seedDir, name string, size int) (metainfo.Info, metainfo.MetaInfo, metainfo.Hash) {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("generating test data: %v", err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, name), data, 0o644); err != nil {
		t.Fatalf("writing seed file: %v", err)
	}

	info := metainfo.Info{PieceLength: 16 * 1024}
	if err := info.BuildFromFilePath(filepath.Join(seedDir, name)); err != nil {
		t.Fatalf("BuildFromFilePath: %v", err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatalf("marshaling info: %v", err)
	}
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	mi.SetDefaults()
	return info, mi, mi.HashInfoBytes()
}

func TestEndToEndDownload(t *testing.T) {
	seedDir := t.TempDir()
	_, mi, infoHash := buildTorrent(t, seedDir, "seed.bin", 300_000)

	wantSum := sha256.Sum256(mustRead(t, filepath.Join(seedDir, "seed.bin")))

	seedClient, err := torrent.NewClient(offlineConfig(seedDir))
	if err != nil {
		t.Fatalf("starting seed client: %v", err)
	}
	defer seedClient.Close()
	seedTorrent, err := seedClient.AddTorrent(&mi)
	if err != nil {
		t.Fatalf("seeding torrent: %v", err)
	}
	<-seedTorrent.GotInfo()
	seedTorrent.DownloadAll() // triggers piece verification against the on-disk data

	// Write the .torrent file the leecher will load (full metainfo, so
	// it needs no metadata exchange — only the actual piece transfer,
	// which is what this test is verifying).
	torrentPath := filepath.Join(t.TempDir(), "seed.torrent")
	f, err := os.Create(torrentPath)
	if err != nil {
		t.Fatalf("creating .torrent file: %v", err)
	}
	if err := mi.Write(f); err != nil {
		t.Fatalf("writing .torrent file: %v", err)
	}
	f.Close()

	leechDir := t.TempDir()
	leechClient, err := torrent.NewClient(offlineConfig(leechDir))
	if err != nil {
		t.Fatalf("starting leech client: %v", err)
	}
	defer leechClient.Close()
	eng := &Engine{client: leechClient}

	dl := &domain.Download{URL: torrentPath, Kind: domain.KindTorrent}
	stats := make(chan manager.TorrentStats, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- eng.Start(ctx, dl.ID, dl, stats) }()

	// Point the leecher at the seeder directly — no DHT/tracker needed.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if lt, ok := leechClient.Torrent(infoHash); ok {
			lt.AddClientPeer(seedClient)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	var lastStats manager.TorrentStats
	for {
		select {
		case st := <-stats:
			lastStats = st
			if st.Done {
				goto downloaded
			}
		case err := <-runErr:
			t.Fatalf("Run returned before completion: %v", err)
		case <-ctx.Done():
			t.Fatalf("timed out waiting for torrent download to complete (last stats: %+v)", lastStats)
		}
	}
downloaded:
	if err := <-runErr; err != nil {
		t.Fatalf("Run returned an error after completion: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(leechDir, "seed.bin"))
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	gotSum := sha256.Sum256(got)
	if gotSum != wantSum {
		t.Errorf("downloaded content checksum mismatch (got %d bytes)", len(got))
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}
