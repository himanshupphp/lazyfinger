//go:build windows

package integrate

import (
	"fmt"
	"os/exec"

	"golang.org/x/sys/windows/registry"
)

// Right-clicking a folder, and right-clicking empty space inside one.
// HKCU means no administrator rights are needed.
var keys = []string{
	`Software\Classes\Directory\shell\LazyFinger`,
	`Software\Classes\Directory\Background\shell\LazyFinger`,
}

func command(exe string) string {
	if wt, err := exec.LookPath("wt.exe"); err == nil {
		return fmt.Sprintf(`"%s" -d "%%V" "%s" run --pause "%%V"`, wt, exe)
	}
	// The outer pair of quotes is stripped by cmd /k; the inner ones survive.
	return fmt.Sprintf(`cmd.exe /k ""%s" run --pause "%%V""`, exe)
}

func install(exe string) ([]string, string, error) {
	cmd := command(exe)
	for _, k := range keys {
		key, _, err := registry.CreateKey(registry.CURRENT_USER, k, registry.ALL_ACCESS)
		if err != nil {
			return nil, "", err
		}
		_ = key.SetStringValue("", label)
		_ = key.SetStringValue("Icon", exe+",0")
		key.Close()

		ck, _, err := registry.CreateKey(registry.CURRENT_USER, k+`\command`, registry.ALL_ACCESS)
		if err != nil {
			return nil, "", err
		}
		err = ck.SetStringValue("", cmd)
		ck.Close()
		if err != nil {
			return nil, "", err
		}
	}
	return []string{"Explorer → right-click a folder, or empty space inside one → " + label},
		`On Windows 11 it's under "Show more options".`, nil
}

func uninstall() ([]string, error) {
	var removed []string
	for _, k := range keys {
		_ = registry.DeleteKey(registry.CURRENT_USER, k+`\command`)
		if err := registry.DeleteKey(registry.CURRENT_USER, k); err == nil {
			removed = append(removed, `HKEY_CURRENT_USER\`+k)
		}
	}
	return removed, nil
}

func installed() []string {
	var out []string
	for _, k := range keys {
		if key, err := registry.OpenKey(registry.CURRENT_USER, k, registry.QUERY_VALUE); err == nil {
			key.Close()
			out = append(out, `HKEY_CURRENT_USER\`+k)
		}
	}
	return out
}
