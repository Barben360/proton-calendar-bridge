package runkey

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

type store struct{ key string }

// System is the Run value of the current user.
func System() Store { return store{KeyPath} }

// At is the value pcb-ui under HKEY_CURRENT_USER\subkey: a test's own key,
// never the real Run key.
func At(subkey string) Store { return store{subkey} }

func (s store) Get() (string, bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, s.key, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer k.Close()
	v, _, err := k.GetStringValue(Value)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (s store) Set(data string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, s.key, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(Value, data)
}

func (s store) Delete() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, s.key, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(Value); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}
