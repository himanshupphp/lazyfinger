//go:build linux

package integrate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func home() string { h, _ := os.UserHomeDir(); return h }

func launcherPath() string {
	return filepath.Join(home(), ".local", "share", "lazyfinger", "open-in-terminal.sh")
}

type entry struct {
	path    string
	desc    string
	manager string // file manager it belongs to
	content func(launcher string) string
}

func entries() []entry {
	script := func(l string) string { return fmt.Sprintf("#!/bin/sh\nexec %s \"${1:-$PWD}\"\n", shQuote(l)) }
	dolphin := func(l string) string {
		return fmt.Sprintf(`[Desktop Entry]
Type=Service
MimeType=inode/directory;
Actions=runWithHeal;
X-KDE-ServiceTypes=KonqPopupMenu/Plugin
X-KDE-Priority=TopLevel

[Desktop Action runWithHeal]
Name=%s
Icon=utilities-terminal
Exec=%s %%f
`, label, l)
	}
	return []entry{
		{filepath.Join(home(), ".local", "share", "nautilus", "scripts", label), "GNOME Files: right-click → Scripts → " + label, "nautilus", script},
		{filepath.Join(home(), ".config", "caja", "scripts", label), "Caja: right-click → Scripts → " + label, "caja", script},
		{filepath.Join(home(), ".local", "share", "nemo", "actions", "lazyfinger-run.nemo_action"), "Nemo: right-click a folder → " + label, "nemo",
			func(l string) string {
				return fmt.Sprintf("[Nemo Action]\nName=%s\nComment=Install, set up and start this project with LazyFinger\nExec=%s %%F\nIcon-Name=utilities-terminal\nSelection=s\nExtensions=dir;\n", label, l)
			}},
		{filepath.Join(home(), ".local", "share", "nemo", "actions", "lazyfinger-run-here.nemo_action"), "Nemo: right-click empty space → " + label, "nemo",
			func(l string) string {
				return fmt.Sprintf("[Nemo Action]\nName=%s\nComment=Install, set up and start this project with LazyFinger\nExec=%s %%P\nIcon-Name=utilities-terminal\nSelection=none\nExtensions=any;\n", label, l)
			}},
		{filepath.Join(home(), ".local", "share", "kio", "servicemenus", "lazyfinger-run.desktop"), "Dolphin: right-click a folder → " + label, "dolphin", dolphin},
		{filepath.Join(home(), ".local", "share", "kservices5", "ServiceMenus", "lazyfinger-run.desktop"), "Dolphin (KDE 5): right-click a folder → " + label, "dolphin", dolphin},
	}
}

const launcher = `#!/bin/sh
# Opens a terminal running "lazy run" in the given folder. Created by lazy integrate.
HEAL=%s
dir="${1:-$PWD}"
[ -d "$dir" ] || dir=$(dirname "$dir")
cd "$dir" || exit 1
if [ -n "$TERMINAL" ] && command -v "$TERMINAL" >/dev/null 2>&1; then exec "$TERMINAL" -e "$HEAL" run --pause "$dir"
elif command -v gnome-terminal >/dev/null 2>&1; then exec gnome-terminal --working-directory="$dir" -- "$HEAL" run --pause "$dir"
elif command -v konsole >/dev/null 2>&1; then exec konsole --workdir "$dir" -e "$HEAL" run --pause "$dir"
elif command -v xfce4-terminal >/dev/null 2>&1; then exec xfce4-terminal --working-directory="$dir" -x "$HEAL" run --pause "$dir"
elif command -v mate-terminal >/dev/null 2>&1; then exec mate-terminal --working-directory="$dir" -x "$HEAL" run --pause "$dir"
elif command -v kitty >/dev/null 2>&1; then exec kitty --directory "$dir" "$HEAL" run --pause "$dir"
elif command -v alacritty >/dev/null 2>&1; then exec alacritty --working-directory "$dir" -e "$HEAL" run --pause "$dir"
elif command -v wezterm >/dev/null 2>&1; then exec wezterm start --cwd "$dir" -- "$HEAL" run --pause "$dir"
elif command -v x-terminal-emulator >/dev/null 2>&1; then exec x-terminal-emulator -e "$HEAL" run --pause "$dir"
elif command -v xterm >/dev/null 2>&1; then exec xterm -e "$HEAL" run --pause "$dir"
fi
command -v notify-send >/dev/null 2>&1 && notify-send "LazyFinger" "No terminal app was found to run LazyFinger in."
exit 1
`

func install(exe string) ([]string, string, error) {
	l := launcherPath()
	if err := writeExecutable(l, fmt.Sprintf(launcher, shQuote(exe))); err != nil {
		return nil, "", err
	}
	var added []string
	for _, e := range entries() {
		if _, err := exec.LookPath(e.manager); err != nil {
			continue // that file manager isn't installed
		}
		if err := writeExecutable(e.path, e.content(l)); err != nil {
			return added, "", err
		}
		added = append(added, e.desc)
	}
	if len(added) == 0 {
		return nil, "", fmt.Errorf("no supported file manager was found (GNOME Files, Nemo, Caja or Dolphin). You can still bind %s to a shortcut", l)
	}
	return added, "You may need to close and reopen the file manager to see it.", nil
}

func uninstall() ([]string, error) {
	var removed []string
	for _, e := range entries() {
		if exists(e.path) && os.Remove(e.path) == nil {
			removed = append(removed, e.desc)
		}
	}
	if exists(launcherPath()) {
		_ = os.Remove(launcherPath())
	}
	return removed, nil
}

func installed() []string {
	var out []string
	for _, e := range entries() {
		if exists(e.path) {
			out = append(out, e.desc)
		}
	}
	return out
}
