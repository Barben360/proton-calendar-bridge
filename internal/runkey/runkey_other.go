//go:build !windows

package runkey

import "errors"

type none struct{}

// System is a store with no value: there is no Run key off Windows.
func System() Store { return none{} }

func (none) Get() (string, bool, error) { return "", false, nil }
func (none) Set(string) error           { return errors.New("the Run key is Windows only") }
func (none) Delete() error              { return errors.New("the Run key is Windows only") }
