// Package integrate adds "Run with LazyFinger" to the file manager's right-click
// menu on macOS, Windows and Linux, and removes it again.
package integrate

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/himanshupphp/lazyfinger/internal/ui"
)

const label = "Run with LazyFinger"

func Run(args []string) int {
	fs := flag.NewFlagSet("lazy integrate", flag.ContinueOnError)
	remove := fs.Bool("remove", false, "remove the right-click menu entries")
	status := fs.Bool("status", false, "show whether the entries are installed")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	switch {
	case *status:
		items := installed()
		if len(items) == 0 {
			ui.Muted("The right-click menu isn't set up. Run `lazy integrate` to add it.")
		}
		for _, it := range items {
			ui.Check(true, it)
		}
		return 0
	case *remove:
		removed, err := uninstall()
		for _, r := range removed {
			ui.Success("Removed " + r)
		}
		if err != nil {
			ui.Error(err.Error())
			return 1
		}
		if len(removed) == 0 {
			ui.Muted("Nothing to remove.")
		}
		return 0
	}

	exe, err := healPath()
	if err != nil {
		ui.Error("Couldn't find the lazy program: " + err.Error())
		return 1
	}
	added, hint, err := install(exe)
	for _, a := range added {
		ui.Success("Added: " + a)
	}
	if err != nil {
		ui.Error(err.Error())
		return 1
	}
	if hint != "" {
		ui.Muted(hint)
	}
	ui.Muted("If you move or reinstall lazy somewhere else, run `lazy integrate` again. To take it out: `lazy integrate --remove`.")
	return 0
}

// healPath prefers the path on PATH (e.g. Homebrew's stable link), so the
// menu keeps working after upgrades.
func healPath() (string, error) {
	if p, err := exec.LookPath("lazy"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs, nil
		}
	}
	return os.Executable()
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func writeExecutable(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o755)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
