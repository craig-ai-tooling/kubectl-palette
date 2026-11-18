//go:build !windows

package session

import (
	"os"
	"syscall"
)

// Lock is an exclusive advisory file lock.
type Lock struct{ f *os.File }

// LockFile blocks until it holds an exclusive lock on path, creating it mode 600.
func LockFile(path string) (*Lock, error) { return lockFile(path, false) }

// TryLockFile returns (nil, nil) when another process holds the lock.
func TryLockFile(path string) (*Lock, error) { return lockFile(path, true) }

func lockFile(path string, try bool) (*Lock, error) {
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if try {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		if try && err == syscall.EWOULDBLOCK {
			return nil, nil
		}
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Unlock releases the lock.
func (l *Lock) Unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
}
