// Command gdm is GDM (Go Download Manager): it
// splits HTTP downloads across multiple connections, resumes them
// after a pause or crash, and drives everything from a terminal
// dashboard.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/wongpinter/gdm/internal/engine"
	"github.com/wongpinter/gdm/internal/manager"
	"github.com/wongpinter/gdm/internal/store"
	"github.com/wongpinter/gdm/internal/torrentengine"
	"github.com/wongpinter/gdm/internal/tui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gdm:", err)
		os.Exit(1)
	}
}

func run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}

	downloadDir := flag.String("dir", filepath.Join(home, "Downloads"), "directory to save files into")
	stateDir := flag.String("state", filepath.Join(home, ".idm", "state"), "directory to persist download state into")
	connections := flag.Int("connections", 4, "default number of connections per download")
	maxActive := flag.Int("max-active", 3, "maximum number of downloads running at once")
	flag.Parse()

	st, err := store.New(*stateDir)
	if err != nil {
		return fmt.Errorf("initializing state store: %w", err)
	}

	eng := engine.New()

	mgr, err := manager.New(eng, st, *downloadDir, *connections, *maxActive)
	if err != nil {
		return fmt.Errorf("initializing manager: %w", err)
	}

	te, err := torrentengine.New(*downloadDir)
	if err != nil {
		// Torrent support is additive — a machine without usable
		// network sockets for it can still run HTTP downloads.
		fmt.Fprintf(os.Stderr, "gdm: torrent support unavailable: %v\n", err)
	} else {
		mgr.SetTorrentEngine(te)
		defer te.Close()
	}

	for _, arg := range flag.Args() {
		var addErr error
		if isTorrentSource(arg) {
			_, addErr = mgr.AddTorrent(arg)
		} else {
			_, addErr = mgr.Add(arg, 0)
		}
		if addErr != nil {
			fmt.Fprintf(os.Stderr, "gdm: adding %s: %v\n", arg, addErr)
		}
	}

	p := tea.NewProgram(tui.New(mgr), tea.WithAltScreen())
	_, runErr := p.Run()

	mgr.Shutdown(10 * time.Second)

	return runErr
}

// isTorrentSource reports whether arg names a torrent (a magnet URI or
// a path to a local .torrent file) rather than an HTTP(S) URL.
func isTorrentSource(arg string) bool {
	return strings.HasPrefix(arg, "magnet:") || strings.HasSuffix(strings.ToLower(arg), ".torrent")
}
