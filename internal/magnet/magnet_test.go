package magnet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

const testHash = "774253cc2983a7479a3d5b2ff91386027a006ebd"

func TestInfoHashOfMagnet(t *testing.T) {
	uri := "magnet:?xt=urn:btih:" + testHash + "&dn=Some+Name&tr=udp://tracker.example:1337/announce"
	got, err := InfoHashOf(uri)
	if err != nil {
		t.Fatalf("InfoHashOf: %v", err)
	}
	if got != testHash {
		t.Fatalf("InfoHashOf = %q, want %q", got, testHash)
	}
}

func TestInfoHashOfMagnetUppercase(t *testing.T) {
	uri := "magnet:?xt=urn:btih:" + strings.ToUpper(testHash)
	got, err := InfoHashOf(uri)
	if err != nil {
		t.Fatalf("InfoHashOf: %v", err)
	}
	if got != testHash {
		t.Fatalf("InfoHashOf = %q, want normalized %q", got, testHash)
	}
}

func TestInfoHashOfInvalid(t *testing.T) {
	if _, err := InfoHashOf("magnet:?dn=nothing"); err == nil {
		t.Fatal("expected error for magnet without xt")
	}
	if _, err := InfoHashOf(filepath.Join(t.TempDir(), "missing.torrent")); err == nil {
		t.Fatal("expected error for missing .torrent file")
	}
}

func TestDisplayNameAndTrackersOfMagnet(t *testing.T) {
	uri := "magnet:?xt=urn:btih:" + testHash + "&dn=Art+of+Foo&tr=udp://one.example:1337/announce&tr=https://two.example/announce"
	if got := DisplayNameOf(uri); got != "Art of Foo" {
		t.Fatalf("DisplayNameOf = %q, want %q", got, "Art of Foo")
	}
	tr := TrackersOf(uri)
	if len(tr) != 2 || tr[0] != "udp://one.example:1337/announce" || tr[1] != "https://two.example/announce" {
		t.Fatalf("TrackersOf = %q", tr)
	}
}

func TestInfoHashOfTorrentFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.torrent")
	info := metainfo.Info{PieceLength: 16384, Length: 3}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatalf("marshal info: %v", err)
	}
	mi := metainfo.MetaInfo{
		InfoBytes:    infoBytes,
		Announce:     "udp://one.example:1337/announce",
		AnnounceList: [][]string{{"udp://two.example:6969/announce"}},
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	if err := mi.Write(f); err != nil {
		f.Close()
		t.Fatalf("write torrent: %v", err)
	}
	f.Close()

	got, err := InfoHashOf(path)
	if err != nil {
		t.Fatalf("InfoHashOf: %v", err)
	}
	if got != strings.ToLower(mi.HashInfoBytes().HexString()) {
		t.Fatalf("InfoHashOf = %q, want file hash", got)
	}
	tr := TrackersOf(path)
	if len(tr) != 2 {
		t.Fatalf("TrackersOf = %q, want announce + announce-list", tr)
	}
}
