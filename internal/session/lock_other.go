//go:build windows

package session

import "os"

// Lock is a no-op on Windows in this prototype: concurrent refreshes are not serialized there.
type Lock struct{ f *os.File }

// LockFile creates the lock file and returns without locking.
func LockFile(path string) (*Lock, error) {
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return nil, err
	}
	return &Lock{}, nil
}

// TryLockFile behaves like LockFile.
func TryLockFile(path string) (*Lock, error) { return LockFile(path) }

// Unlock does nothing.
func (l *Lock) Unlock() {}
