// Package console owns stdin. A single goroutine reads it and routes bytes to
// the running command when one is attached, or buffers them for LazyFinger's own
// prompts otherwise, so neither side steals keystrokes from the other.
package console

import (
	"io"
	"os"
	"sync"

	"golang.org/x/term"
)

var (
	mu      sync.Mutex
	cond    = sync.NewCond(&mu)
	buf     []byte
	sink    io.Writer
	eof     bool
	started sync.Once
)

func start() {
	started.Do(func() {
		go func() {
			b := make([]byte, 4096)
			for {
				n, err := os.Stdin.Read(b)
				mu.Lock()
				if n > 0 {
					if sink != nil {
						_, _ = sink.Write(b[:n])
					} else {
						buf = append(buf, b[:n]...)
					}
				}
				if err != nil {
					eof = true
				}
				cond.Broadcast()
				mu.Unlock()
				if err != nil {
					return
				}
			}
		}()
	})
}

// Attach sends keyboard input to w until the returned function is called.
// Stray keys typed earlier are discarded so they don't leak into the command.
func Attach(w io.Writer) (detach func()) {
	start()
	mu.Lock()
	buf, sink = nil, w
	mu.Unlock()
	return func() {
		mu.Lock()
		sink = nil
		mu.Unlock()
	}
}

type reader struct{}

func (reader) Read(p []byte) (int, error) {
	start()
	mu.Lock()
	defer mu.Unlock()
	for len(buf) == 0 && !eof {
		cond.Wait()
	}
	if len(buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, buf)
	buf = buf[n:]
	return n, nil
}

// Stdin is what LazyFinger's own prompts read from.
var Stdin io.Reader = reader{}

// MakeRaw switches the terminal to raw mode, if stdin is a terminal, and
// returns a function that restores it.
func MakeRaw() (restore func()) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return func() {}
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return func() {}
	}
	return func() { _ = term.Restore(fd, state) }
}
