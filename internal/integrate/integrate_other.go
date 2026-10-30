//go:build !darwin && !linux && !windows

package integrate

import "errors"

func install(string) ([]string, string, error) {
	return nil, "", errors.New("right-click integration isn't supported on this system yet")
}
func uninstall() ([]string, error) { return nil, nil }
func installed() []string          { return nil }
