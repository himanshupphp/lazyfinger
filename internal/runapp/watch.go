package runapp

import (
	"bytes"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/himanshupphp/lazyfinger/internal/gather"
)

var (
	urlRe      = regexp.MustCompile(`https?://(?:localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1?\])(?::\d{2,5})?(?:/[^\s\])'"]*)?`)
	portWordRe = regexp.MustCompile(`(?i)\b(?:started|listening|running|serving)\b.{0,60}?\bport\(?s?\)?:?\s*(\d{2,5})`)
	busyRe     = regexp.MustCompile(`(?i)(EADDRINUSE|address already in use|port \d+ is (?:already )?in use|failed to listen on|only one usage of each socket address)`)
	runErrRe   = regexp.MustCompile(`(?i)(\berror\b|exception|traceback|unhandled|failed to compile|\bfatal\b|panic:|ERR!)`)
	benignRe   = regexp.MustCompile(`(?i)\b(?:0|no) errors?\b|without errors`)
	logErrRe   = regexp.MustCompile(`(?m)^\[\d{4}-\d{2}-\d{2}[^\]]*\] [\w-]+\.(?:ERROR|CRITICAL|ALERT|EMERGENCY):`)
	logAnyRe   = regexp.MustCompile(`(?m)^\[\d{4}-\d{2}-\d{2}[^\]]*\] [\w-]+\.\w+:`)
)

// watcher reads a running app's output to find its address, notice errors,
// and spot "port already in use".
type watcher struct {
	mu       sync.Mutex
	partial  []byte
	url      string
	ready    chan string
	portLine string
	burst    []string
	capture  int
	lastLine time.Time
	errors   []string
	seen     map[string]bool
	done     chan struct{}
	stopOnce sync.Once
	onError  func(headline string)
}

func newWatcher() *watcher {
	w := &watcher{ready: make(chan string, 1), seen: map[string]bool{}, done: make(chan struct{})}
	go w.flushLoop()
	return w
}

func (w *watcher) stop() { w.stopOnce.Do(func() { close(w.done) }) }

func (w *watcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, p...)
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(gather.Clean(w.partial[:i]), "\r ")
		w.partial = w.partial[i+1:]
		w.line(line)
	}
	if len(w.partial) > 64<<10 {
		w.partial = append([]byte(nil), w.partial[len(w.partial)-(64<<10):]...)
	}
	return len(p), nil
}

func (w *watcher) line(l string) {
	w.lastLine = time.Now()
	if w.url == "" {
		if u := urlRe.FindString(l); u != "" {
			w.setURL(normalizeURL(u))
		} else if m := portWordRe.FindStringSubmatch(l); m != nil {
			w.setURL("http://localhost:" + m[1])
		}
	}
	if busyRe.MatchString(l) {
		w.portLine = l
	}
	if runErrRe.MatchString(l) && !benignRe.MatchString(l) {
		if w.capture == 0 {
			w.burst = nil
		}
		w.capture = 25 // keep the stack trace that usually follows
	}
	if w.capture > 0 {
		w.burst = append(w.burst, l)
		if w.capture--; w.capture == 0 {
			w.finishBurst()
		}
	}
}

func (w *watcher) setURL(u string) {
	w.url = u
	select {
	case w.ready <- u:
	default:
	}
}

// flushLoop ends an error burst once the app has been quiet for a moment.
func (w *watcher) flushLoop() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-w.done:
			w.mu.Lock()
			if w.capture > 0 {
				w.finishBurst()
			}
			w.mu.Unlock()
			return
		case <-t.C:
			w.mu.Lock()
			if w.capture > 0 && time.Since(w.lastLine) > 1500*time.Millisecond {
				w.finishBurst()
			}
			w.mu.Unlock()
		}
	}
}

func (w *watcher) finishBurst() {
	text := strings.TrimSpace(strings.Join(w.burst, "\n"))
	w.burst, w.capture = nil, 0
	if text != "" {
		w.record(text)
	}
}

func (w *watcher) record(text string) {
	fp := gather.Fingerprint(text)
	if w.seen[fp] {
		return
	}
	w.seen[fp] = true
	w.errors = append(w.errors, text)
	if w.onError != nil {
		w.onError(headline(text))
	}
}

func (w *watcher) addLogEntry(text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.record(text)
}

func (w *watcher) errorList() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.errors...)
}

func normalizeURL(u string) string {
	u = strings.Replace(u, "0.0.0.0", "localhost", 1)
	u = strings.Replace(u, "[::]", "localhost", 1)
	return strings.TrimRight(u, "/.")
}

func headline(text string) string {
	lines := strings.Split(text, "\n")
	pick := strings.TrimSpace(lines[0])
	for _, l := range lines {
		if runErrRe.MatchString(l) {
			pick = strings.TrimSpace(l)
			break
		}
	}
	if len(pick) > 90 {
		pick = pick[:90] + "..."
	}
	return pick
}

// waitReady polls until something accepts connections at the URL's address.
func waitReady(raw string, stop <-chan struct{}) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Host
	if u.Port() == "" {
		host += ":80"
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-stop:
			return false
		default:
		}
		if c, err := net.DialTimeout("tcp", host, time.Second); err == nil {
			c.Close()
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// tailLaravelLog watches storage/logs for new error entries while the app
// runs, because Laravel writes most request errors there, not to the terminal.
func tailLaravelLog(root string, w *watcher, stop <-chan struct{}) {
	offsets := map[string]int64{}
	files := func() []string {
		m, _ := filepath.Glob(filepath.Join(root, "storage", "logs", "laravel*.log"))
		return m
	}
	for _, f := range files() {
		if fi, err := os.Stat(f); err == nil {
			offsets[f] = fi.Size() // only report errors from now on
		}
	}
	for {
		select {
		case <-stop:
			return
		case <-time.After(2 * time.Second):
		}
		for _, f := range files() {
			fi, err := os.Stat(f)
			if err != nil {
				continue
			}
			off := offsets[f] // 0 for a file created after we started
			if fi.Size() < off {
				off = 0 // rotated
			}
			if fi.Size() == off {
				continue
			}
			if fi.Size()-off > 256<<10 {
				off = fi.Size() - 256<<10
			}
			buf := make([]byte, fi.Size()-off)
			fh, err := os.Open(f)
			if err != nil {
				continue
			}
			n, _ := fh.ReadAt(buf, off)
			fh.Close()
			offsets[f] = off + int64(n)
			text := string(buf[:n])
			starts := logAnyRe.FindAllStringIndex(text, -1)
			for _, e := range logErrRe.FindAllStringIndex(text, -1) {
				end := len(text)
				for _, s := range starts {
					if s[0] > e[0] {
						end = s[0]
						break
					}
				}
				entry := strings.Split(strings.TrimSpace(text[e[0]:end]), "\n")
				if len(entry) > 40 {
					entry = entry[:40]
				}
				w.addLogEntry(strings.Join(entry, "\n"))
			}
		}
	}
}
