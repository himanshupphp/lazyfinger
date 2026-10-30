//go:build windows

package runner

import (
	"context"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/UserExistsError/conpty"
	"golang.org/x/term"

	"github.com/himanshupphp/lazyfinger/internal/console"
)

func RunWith(dir string, args []string, extra io.Writer) (*Result, error) {
	if conpty.IsConPtyAvailable() { // Windows 10 1809 and later
		return runConPTY(dir, args, extra)
	}
	return runPipes(dir, args, extra)
}

// commandLine goes through cmd.exe so npm.cmd, composer.bat and friends resolve
// the same way they do when you type them.
func commandLine(args []string) string {
	if shellish(args) {
		return `cmd.exe /d /s /c "` + args[0] + `"`
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = syscall.EscapeArg(a)
	}
	return `cmd.exe /d /s /c "` + strings.Join(quoted, " ") + `"`
}

func runConPTY(dir string, args []string, extra io.Writer) (*Result, error) {
	w, h := 120, 40
	if tw, th, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		w, h = tw, th
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}
	cpty, err := conpty.Start(commandLine(args), conpty.ConPtyDimensions(w, h), conpty.ConPtyWorkDir(dir))
	if err != nil {
		return runPipes(dir, args, extra)
	}
	var closeOnce sync.Once
	closePTY := func() { closeOnce.Do(func() { _ = cpty.Close() }) }
	defer closePTY()

	keys := &keyWatcher{w: cpty}
	restore := console.MakeRaw()
	detach := console.Attach(keys)
	defer func() { detach(); restore() }()

	stopResize := watchSize(func(w, h int) { _ = cpty.Resize(w, h) })
	defer stopResize()

	out := newTail(maxOutput)
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(outputs(out, extra), cpty)
		close(done)
	}()

	code, err := cpty.Wait(context.Background())
	drain(done, closePTY)
	if err != nil {
		return nil, err
	}
	return &Result{ExitCode: int(code), Output: out.Bytes(), Interrupted: keys.ctrlC.Load()}, nil
}

// Windows has no resize signal, so poll the window size.
func watchSize(resize func(w, h int)) (stop func()) {
	quit := make(chan struct{})
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		lw, lh := -1, -1
		for {
			select {
			case <-quit:
				return
			case <-t.C:
				w, h, err := term.GetSize(int(os.Stdout.Fd()))
				if err != nil || (w == lw && h == lh) {
					continue
				}
				if lw != -1 {
					resize(w, h)
				}
				lw, lh = w, h
			}
		}
	}()
	return func() { close(quit) }
}

// runPipes is the fallback for Windows versions without ConPTY.
func runPipes(dir string, args []string, extra io.Writer) (*Result, error) {
	cmd := exec.Command("cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: commandLine(args)}
	cmd.Dir = dir

	out := newTail(maxOutput)
	w := outputs(out, extra)
	cmd.Stdout, cmd.Stderr = w, w
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}

	var interrupted atomic.Bool
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	go func() {
		for range sigs {
			interrupted.Store(true)
		}
	}()
	defer func() { signal.Stop(sigs); close(sigs) }()

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	detach := console.Attach(stdin)
	err = cmd.Wait()
	detach()
	return finish(&Result{Output: out.Bytes(), Interrupted: interrupted.Load()}, err)
}
