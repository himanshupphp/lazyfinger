package editor

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Command picks the editor for reviewing a fix: $VISUAL, then $EDITOR, then
// VS Code if installed, then a sensible default for the platform.
// GUI editors must block until closed (e.g. EDITOR="code --wait").
func Command() []string {
	for _, env := range []string{"VISUAL", "EDITOR"} {
		if v := strings.Fields(os.Getenv(env)); len(v) > 0 {
			return v
		}
	}
	if _, err := exec.LookPath("code"); err == nil {
		return []string{"code", "--wait"}
	}
	if runtime.GOOS == "windows" {
		return []string{"notepad"}
	}
	for _, e := range []string{"nano", "vim", "vi"} {
		if _, err := exec.LookPath(e); err == nil {
			return []string{e}
		}
	}
	return []string{"vi"}
}
