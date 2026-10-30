package runner

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxOutput = 256 << 10 // keep the last 256 KB of output

type Result struct {
	ExitCode    int
	Output      []byte
	Interrupted bool // user pressed Ctrl-C
}

func (r *Result) Failed() bool { return !r.Interrupted && r.ExitCode != 0 }

// Run runs a command attached to the terminal and returns its result.
func Run(dir string, args []string) (*Result, error) { return RunWith(dir, args, nil) }

// outputs sends command output to the terminal, the kept tail, and an
// optional extra writer (lazy run's watcher).
func outputs(t *tail, extra io.Writer) io.Writer {
	ws := []io.Writer{os.Stdout, t}
	if extra != nil {
		ws = append(ws, extra)
	}
	return io.MultiWriter(ws...)
}

// tail is an io.Writer that keeps only the last max bytes.
type tail struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTail(max int) *tail { return &tail{max: max} }

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

func (t *tail) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.buf...)
}

// keyWatcher forwards keystrokes to the command and notes whether Ctrl-C was pressed.
type keyWatcher struct {
	w     io.Writer
	ctrlC atomic.Bool
}

func (k *keyWatcher) Write(p []byte) (int, error) {
	if bytes.IndexByte(p, 0x03) >= 0 {
		k.ctrlC.Store(true)
	}
	return k.w.Write(p)
}

// shellish reports whether a single quoted argument like "npm test && npm run build"
// should go through the shell.
func shellish(args []string) bool {
	return len(args) == 1 && strings.ContainsAny(args[0], " \t&|;<>$`")
}

// drain gives the output copier a moment to finish after the command exits,
// then closes the terminal to unblock it if something still holds it open.
func drain(done <-chan struct{}, closeFn func()) {
	select {
	case <-done:
		return
	case <-time.After(500 * time.Millisecond):
	}
	closeFn()
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

func finish(res *Result, err error) (*Result, error) {
	if err == nil {
		return res, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		if res.ExitCode < 0 { // killed by a signal
			res.ExitCode = 1
		}
		return res, nil
	}
	return nil, err
}
