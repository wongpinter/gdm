// Package magnet parses torrent identity out of magnet URIs and
// .torrent files without touching the network. The normalized info
// hash is the stable identity of a torrent: the same payload with a
// different tracker list or display name still shares one info hash,
// so it drives dedupe and metainfo-cache filenames.
package magnet

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/anacrolix/torrent/metainfo"
)

// InfoHashOf returns the lowercase hex info hash for a magnet URI or
// a path to a .torrent file. Unknown inputs yield an error; callers
// treat empty hash as "no stable identity" and fall back to URL match.
func InfoHashOf(uri string) (string, error) {
	if strings.HasPrefix(uri, "magnet:") {
		m, err := metainfo.ParseMagnetUri(uri)
		if err != nil {
			return "", fmt.Errorf("parsing magnet: %w", err)
		}
		return strings.ToLower(m.InfoHash.HexString()), nil
	}
	mi, err := metainfo.LoadFromFile(uri)
	if err != nil {
		return "", fmt.Errorf("loading torrent file: %w", err)
	}
	return strings.ToLower(mi.HashInfoBytes().HexString()), nil
}

// DisplayNameOf returns the dn parameter of a magnet URI, or "".
func DisplayNameOf(uri string) string {
	if !strings.HasPrefix(uri, "magnet:") {
		return ""
	}
	m, err := metainfo.ParseMagnetUri(uri)
	if err != nil {
		return ""
	}
	return m.DisplayName
}

// TrackersOf returns the tracker URLs advertised by a magnet URI
// (tr parameters) or a .torrent file (announce + announce-list).
func TrackersOf(uri string) []string {
	if strings.HasPrefix(uri, "magnet:") {
		m, err := metainfo.ParseMagnetUri(uri)
		if err != nil {
			return nil
		}
		return append([]string(nil), m.Trackers...)
	}
	mi, err := metainfo.LoadFromFile(uri)
	if err != nil {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	add(mi.Announce)
	for _, tier := range mi.AnnounceList {
		for _, u := range tier {
			add(u)
		}
	}
	return out
}

// QueryParam returns the first value of a magnet query parameter.
func QueryParam(uri, key string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	return u.Query().Get(key)
}
