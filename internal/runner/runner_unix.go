//go:build !windows

package runner

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/creack/pty"

	"github.com/himanshupphp/lazyfinger/internal/console"
)

func RunWith(dir string, args []string, extra io.Writer) (*Result, error) {
	var cmd *exec.Cmd
	if shellish(args) {
		cmd = exec.Command("sh", "-c", args[0])
	} else {
		cmd = exec.Command(args[0], args[1:]...)
	}
	cmd.Dir = dir

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	var closeOnce sync.Once
	closePTY := func() { closeOnce.Do(func() { _ = ptmx.Close() }) }
	defer closePTY()
	if err := pty.InheritSize(os.Stdin, ptmx); err != nil {
		_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 40, Cols: 120})
	}

	keys := &keyWatcher{w: ptmx}
	restore := console.MakeRaw()
	detach := console.Attach(keys)
	defer func() { detach(); restore() }()

	var interrupted atomic.Bool
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGWINCH)
	go func() {
		for s := range sigs {
			if s == syscall.SIGWINCH {
				_ = pty.InheritSize(os.Stdin, ptmx)
				continue
			}
			interrupted.Store(true)
			_ = syscall.Kill(-cmd.Process.Pid, s.(syscall.Signal))
		}
	}()
	defer func() { signal.Stop(sigs); close(sigs) }()

	out := newTail(maxOutput)
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(outputs(out, extra), ptmx) // EIO on Linux at exit is expected
		close(done)
	}()

	err = cmd.Wait()
	drain(done, closePTY)
	return finish(&Result{Output: out.Bytes(), Interrupted: interrupted.Load() || keys.ctrlC.Load()}, err)
}
