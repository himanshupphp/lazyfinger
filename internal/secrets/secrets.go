// Package secrets stores API keys in the operating system's keychain:
// macOS Keychain, Windows Credential Manager, or Secret Service on Linux.
package secrets

import (
	"errors"

	"github.com/zalando/go-keyring"
)

const (
	service       = "lazyfinger"
	legacyService = "he" + "al" // the tool's old name; built at runtime so renames skip it
)

// Get returns a provider's key, moving it over from the old keychain entry
// the first time it's needed.
func Get(provider string) (string, error) {
	v, err := keyring.Get(service, provider)
	if err == nil {
		return v, nil
	}
	if old, oerr := keyring.Get(legacyService, provider); oerr == nil && old != "" {
		if keyring.Set(service, provider, old) == nil {
			_ = keyring.Delete(legacyService, provider)
		}
		return old, nil
	}
	return v, err
}

func Set(provider, key string) error { return keyring.Set(service, provider, key) }

func Delete(provider string) error {
	_ = keyring.Delete(legacyService, provider)
	if err := keyring.Delete(service, provider); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}
