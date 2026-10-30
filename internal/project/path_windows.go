//go:build windows

package project

import (
	"os"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// RefreshPath reloads PATH from the registry so a tool installed a moment ago
// (for example with winget) is found without restarting the terminal.
func RefreshPath() {
	var parts []string
	for _, k := range []struct {
		root registry.Key
		path string
	}{
		{registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`},
		{registry.CURRENT_USER, `Environment`},
	} {
		key, err := registry.OpenKey(k.root, k.path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		v, _, err := key.GetStringValue("Path")
		key.Close()
		if err == nil && v != "" {
			parts = append(parts, v)
		}
	}
	if len(parts) == 0 {
		return
	}
	joined := strings.Join(parts, ";")
	if expanded, err := registry.ExpandString(joined); err == nil {
		joined = expanded
	}
	_ = os.Setenv("PATH", joined)
}
