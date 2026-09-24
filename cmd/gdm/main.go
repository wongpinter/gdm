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

	"github.com/wongpinter/gdm/internal/domain"
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
	interval := flag.Duration("interval", 2*time.Second, "headless progress refresh interval (e.g. 500ms for smoother bars)")
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

	var watchIDs []string
	for _, arg := range flag.Args() {
		if id, ok := reuseExisting(mgr, arg); ok {
			watchIDs = append(watchIDs, id)
			continue
		}
		var added *domain.Download
		var addErr error
		if isTorrentSource(arg) {
			added, addErr = mgr.AddTorrent(arg)
		} else {
			added, addErr = mgr.Add(arg, 0)
		}
		if addErr != nil {
			fmt.Fprintf(os.Stderr, "gdm: adding %s: %v\n", arg, addErr)
			continue
		}
		watchIDs = append(watchIDs, added.ID)
	}

	p := tea.NewProgram(tui.New(mgr), tea.WithAltScreen())
	var runErr error
	if *headless || !hasTTY() {
		if !*headless {
			fmt.Fprintln(os.Stderr, "gdm: no TTY detected, falling back to --headless mode")
		}
		runErr = runHeadless(mgr, watchIDs, *interval)
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

// reuseExisting returns the ID of an already-known download with the
// same URL instead of queueing a duplicate. A failed/canceled/paused
// match is resumed so a re-run retries it; an active or completed match
// is just watched. ok=false means no match — caller should Add.
func reuseExisting(mgr *manager.Manager, url string) (id string, ok bool) {
	for _, s := range mgr.List() {
		d := s.Download
		if d == nil || d.URL != url {
			continue
		}
		switch d.Status {
		case domain.StatusFailed, domain.StatusCanceled, domain.StatusPaused:
			if err := mgr.Resume(d.ID); err != nil {
				fmt.Fprintf(os.Stderr, "gdm: resuming %s: %v\n", url, err)
				return d.ID, true
			}
			fmt.Fprintf(os.Stderr, "gdm: resuming existing %s (%s)\n", url, d.Status)
		default:
			fmt.Fprintf(os.Stderr, "gdm: already queued %s (%s)\n", url, d.Status)
		}
		return d.ID, true
	}
	return "", false
}

// runHeadless blocks until every watched download reaches a terminal
// state, rewriting the same lines in place instead of appending, so
// notebook / CI logs stay at N lines. It returns a non-nil error if
// any watched download failed or was canceled, so scripts and
// notebooks can detect failure via exit code.
func runHeadless(mgr *manager.Manager, watchIDs []string, interval time.Duration) error {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ids := watchedUnfinished(mgr, watchIDs)
	if len(ids) == 0 {
		if len(watchIDs) == 0 {
			fmt.Fprintln(os.Stderr, "gdm: nothing queued (pass a URL, magnet URI, or .torrent path)")
		}
		return headlessResult(mgr, watchIDs)
	}
	if len(ids) == 1 {
		return runHeadlessSingle(mgr, ids[0], interval)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	prev := printHeadlessBlock(mgr, ids)
	for range ticker.C {
		fmt.Printf("\033[%dA", prev)
		prev = printHeadlessBlock(mgr, ids)
		if allFinished(mgr, ids) {
			return headlessResult(mgr, ids)
		}
	}
	return nil
}

// runHeadlessSingle follows one download on a single \r-rewritten
// line. Colab / Jupyter cells render carriage-return updates in
// place, so this shows as a live progress bar instead of a growing
// log — the common Colab case is exactly one magnet per cell.
func runHeadlessSingle(mgr *manager.Manager, id string, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	fmt.Print(barLine(mgr, id))
	for range ticker.C {
		fmt.Printf("\r\033[K%s", barLine(mgr, id))
		if allFinished(mgr, []string{id}) {
			fmt.Println()
			return headlessResult(mgr, []string{id})
		}
	}
	return nil
}

// watchedUnfinished resolves which IDs to follow: the ones added /
// reused this run, minus any already terminal (a re-run of a
// completed magnet returns immediately). With no args it follows all
// unfinished known downloads so a bare `--headless` still drains the
// queue instead of exiting.
func watchedUnfinished(mgr *manager.Manager, watchIDs []string) []string {
	if len(watchIDs) == 0 {
		var ids []string
		for _, s := range mgr.List() {
			if s.Download != nil && !s.Download.Finished() {
				ids = append(ids, s.Download.ID)
			}
		}
		return ids
	}
	var ids []string
	for _, id := range watchIDs {
		s, ok := mgr.Get(id)
		if !ok || s.Download == nil || s.Download.Finished() {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

func allFinished(mgr *manager.Manager, ids []string) bool {
	for _, id := range ids {
		s, ok := mgr.Get(id)
		if !ok || s.Download == nil || !s.Download.Finished() {
			return false
		}
	}
	return true
}

// printHeadlessBlock renders exactly one bar line per watched
// download (errors inline, truncated) and returns the line count so
// the next tick can move the cursor back up and overwrite. One line
// per download, always — variable line counts would desync rewrite.
func printHeadlessBlock(mgr *manager.Manager, ids []string) int {
	for _, id := range ids {
		fmt.Printf("\r\033[K%s\n", truncRunes(barLine(mgr, id), 200))
	}
	return len(ids)
}

// barLine renders one download as a tqdm-style bar line:
// [downloading] name [██████░░░░] 45.2% 1.4G/3.0G 5.2M/s ETA 10m12s peers=2
func barLine(mgr *manager.Manager, id string) string {
	s, ok := mgr.Get(id)
	if !ok || s.Download == nil {
		return "[gone] " + id
	}
	d := s.Download
	name := d.Filename
	if name == "" {
		name = shortURL(d.URL)
	}
	if r := []rune(name); len(r) > 50 {
		name = string(r[:47]) + "..."
	}
	var prog string
	if d.TotalSize > 0 {
		prog = fmt.Sprintf("%s %5.1f%% %s/%s", bar(d.Progress(), 30),
			d.Progress()*100, humanBytes(float64(d.BytesDownloaded())), humanBytes(float64(d.TotalSize)))
	} else {
		prog = fmt.Sprintf("%s downloaded", humanBytes(float64(d.BytesDownloaded())))
	}
	line := fmt.Sprintf("[%s] %s %s %s/s ETA %s peers=%d",
		d.Status, name, prog, humanBytes(s.SpeedBps), eta(s, d), s.Peers)
	if d.Error != "" {
		line += " ! " + d.Error
	}
	return truncRunes(line, 200)
}

func bar(p float64, width int) string {
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	filled := int(p*float64(width) + 0.5)
	return "[" + strings.Repeat("\u2588", filled) + strings.Repeat("\u2591", width-filled) + "]"
}

func humanBytes(b float64) string {
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1fT", b/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%.1fG", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fM", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1fK", b/(1<<10))
	default:
		return fmt.Sprintf("%.0fB", b)
	}
}

func eta(s manager.Snapshot, d *domain.Download) string {
	if d.TotalSize <= 0 || s.SpeedBps <= 0 {
		return "--"
	}
	remain := float64(d.TotalSize - d.BytesDownloaded())
	if remain <= 0 {
		return "0s"
	}
	return (time.Duration(remain/s.SpeedBps) * time.Second).Truncate(time.Second).String()
}

func truncRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-3]) + "..."
	}
	return s
}

func shortURL(u string) string {
	if len(u) <= 80 {
		return u
	}
	if i := strings.Index(u, "&dn="); i > 0 {
		return u[:i] + "..."
	}
	return u[:77] + "..."
}

func headlessResult(mgr *manager.Manager, ids []string) error {
	var failed []string
	for _, id := range ids {
		s, ok := mgr.Get(id)
		if !ok || s.Download == nil {
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
