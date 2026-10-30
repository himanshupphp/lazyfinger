//go:build !windows

package project

import (
	"os"
	"path/filepath"
	"strings"
)

// RefreshPath makes sure Homebrew's folders are on PATH, so a tool installed
// a moment ago is found.
func RefreshPath() {
	path := os.Getenv("PATH")
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin", filepath.Join(os.Getenv("HOME"), ".cargo", "bin")} {
		if _, err := os.Stat(dir); err == nil && !strings.Contains(":"+path+":", ":"+dir+":") {
			path = dir + ":" + path
		}
	}
	_ = os.Setenv("PATH", path)
}
