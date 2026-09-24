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
	stateDir := flag.String("state", resolveStateDir(home), "directory to persist download state into")
	connections := flag.Int("connections", 4, "default number of connections per download")
	maxActive := flag.Int("max-active", 3, "maximum number of downloads running at once")
	headless := flag.Bool("headless", false, "run without TUI: print progress to stdout and wait until queued downloads finish (for Colab / CI / no-TTY)")
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
	var runErr error
	if *headless || !hasTTY() {
		if !*headless {
			fmt.Fprintln(os.Stderr, "gdm: no TTY detected, falling back to --headless mode")
		}
		runErr = runHeadless(mgr)
	} else {
		_, runErr = p.Run()
	}

	mgr.Shutdown(10 * time.Second)

	return runErr
}

// hasTTY reports whether /dev/tty can be opened. Colab, CI runners,
// and redirected shells have no controlling terminal, so bubbletea
// fails with "could not open a new TTY". Callers use this to fall
// back to headless mode instead of crashing.
func hasTTY() bool {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// runHeadless blocks until every known download reaches a terminal
// state, printing one status line per download every 2s. It returns a
// non-nil error if any download failed or was canceled, so scripts
// and notebooks can detect failure via exit code.
func runHeadless(mgr *manager.Manager) error {
	if len(mgr.List()) == 0 {
		fmt.Fprintln(os.Stderr, "gdm: nothing queued (pass a URL, magnet URI, or .torrent path)")
		return nil
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	printHeadlessStatus(mgr)
	for range ticker.C {
		snaps := mgr.List()
		printHeadlessStatus(mgr)
		allDone := true
		for _, s := range snaps {
			if s.Download == nil || !s.Download.Finished() {
				allDone = false
				break
			}
		}
		if allDone {
			return headlessResult(snaps)
		}
	}
	return nil
}

func printHeadlessStatus(mgr *manager.Manager) {
	for _, s := range mgr.List() {
		d := s.Download
		if d == nil {
			continue
		}
		name := d.Filename
		if name == "" {
			name = d.URL
		}
		if len(name) > 80 {
			name = name[:77] + "..."
		}
		pct := d.Progress() * 100
		fmt.Printf("[%s] %s %.1f%% (%d/%d bytes) peers=%d speed=%.0f B/s\n",
			d.Status, name, pct, d.BytesDownloaded(), d.TotalSize, s.Peers, s.SpeedBps)
		if d.Error != "" {
			fmt.Printf("  error: %s\n", d.Error)
		}
	}
}

func headlessResult(snaps []manager.Snapshot) error {
	var failed []string
	for _, s := range snaps {
		if s.Download == nil {
			continue
		}
		switch s.Download.Status {
		case "failed", "canceled":
			name := s.Download.Filename
			if name == "" {
				name = s.Download.URL
			}
			msg := string(s.Download.Status)
			if s.Download.Error != "" {
				msg += ": " + s.Download.Error
			}
			failed = append(failed, name+" ("+msg+")")
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d download(s) failed: %s", len(failed), strings.Join(failed, "; "))
	}
	fmt.Println("gdm: all downloads finished")
	return nil
}

// isTorrentSource reports whether arg names a torrent (a magnet URI or
// a path to a local .torrent file) rather than an HTTP(S) URL.
func isTorrentSource(arg string) bool {
	return strings.HasPrefix(arg, "magnet:") || strings.HasSuffix(strings.ToLower(arg), ".torrent")
}

// resolveStateDir returns ~/.gdm/state — the state directory since the
// project was renamed from idm to gdm. A one-time migration moves the
// legacy ~/.idm/state into place so existing queues survive the rename
// (the maintainer skill requires an explicit migration plan). If the
// move fails for any reason, the legacy path is used instead rather
// than starting over with an empty queue.
func resolveStateDir(home string) string {
	fresh := filepath.Join(home, ".gdm", "state")
	legacy := filepath.Join(home, ".idm", "state")
	if dirExists(fresh) {
		return fresh // already migrated (or explicitly chosen via -state)
	}
	if !dirExists(legacy) {
		return fresh // nothing to migrate
	}
	if err := os.MkdirAll(filepath.Dir(fresh), 0o755); err != nil {
		return legacy
	}
	if err := os.Rename(legacy, fresh); err != nil {
		return legacy
	}
	return fresh
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
