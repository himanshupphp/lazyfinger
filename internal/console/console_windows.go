//go:build windows

package console

import (
	"os"

	"golang.org/x/sys/windows"
)

// Init turns on ANSI color handling for older consoles (Windows Terminal already has it).
func Init() {
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		h := windows.Handle(f.Fd())
		var mode uint32
		if windows.GetConsoleMode(h, &mode) == nil {
			_ = windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
		}
	}
}
