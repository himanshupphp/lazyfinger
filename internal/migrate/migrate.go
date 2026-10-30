// Package migrate moves settings and project data from the tool's old name
// ("heal") to LazyFinger's, once, so nothing is lost after the rename.
package migrate

import (
	"os"
	"path/filepath"

	"github.com/himanshupphp/lazyfinger/internal/config"
	"github.com/himanshupphp/lazyfinger/internal/ui"
)

// The old name is built at runtime so search-and-replace tools leave it alone.
const oldName = "he" + "al"

// Run moves the user's settings folder and this project's data folder and
// config file, if they still use the old names and the new ones don't exist yet.
func Run(projectRoot string) {
	if newDir, err := config.Dir(); err == nil {
		oldDir := filepath.Join(filepath.Dir(newDir), oldName)
		if move(oldDir, newDir) {
			ui.Muted("Moved your settings from " + oldDir + " to " + newDir + ".")
		}
	}
	if projectRoot == "" {
		return
	}
	if move(filepath.Join(projectRoot, "."+oldName), filepath.Join(projectRoot, ".lazyfinger")) {
		ui.Muted("Moved this project's history and undo snapshots to .lazyfinger/.")
	}
	move(filepath.Join(projectRoot, "."+oldName+".toml"), filepath.Join(projectRoot, ".lazyfinger.toml"))
}

func move(from, to string) bool {
	if _, err := os.Stat(from); err != nil {
		return false
	}
	if _, err := os.Stat(to); err == nil {
		return false // never overwrite data that's already there
	}
	_ = os.MkdirAll(filepath.Dir(to), 0o755)
	return os.Rename(from, to) == nil
}
