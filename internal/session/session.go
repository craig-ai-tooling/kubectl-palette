// Package session stores the login session and keeps it fresh.
//
// The session is an ID token (cluster access) and a refresh token (cluster access renewal
// and the Palette API session). Palette rotates the refresh token on every refresh, so a
// refresh is only safe if the new pair reaches disk before anything else happens. All
// refreshes run under a file lock and re-read the file first, so two concurrent callers
// cannot both spend the same refresh token.
package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/craig-ai-tooling/kubectl-palette/internal/config"
)

// RefreshMargin is how long before expiry an ID token is renewed.
const RefreshMargin = 60 * time.Second

// StaleGrace is the safety margin kept when a cached ID token is used because the refresh
// could not reach Palette.
const StaleGrace = 10 * time.Second

// ErrNoSession means the user has to log in again.
var ErrNoSession = errors.New("no valid session: run: kubectl palette login")

// ErrSessionGone is returned by a Refresher when the issuer rejects the refresh token.
var ErrSessionGone = errors.New("session expired: run: kubectl palette login")

// Session is what is persisted.
type Session struct {
	IDToken      string    `json:"id_token"`
	RefreshToken string    `json:"refresh_token"`
	IDExpiry     time.Time `json:"id_expiry"`
}

// Refresher exchanges a refresh token for a new pair. It must return ErrSessionGone
// (possibly wrapped) when the refresh token is no longer valid.
type Refresher interface {
	Refresh(ctx context.Context, refreshToken string) (idToken, newRefreshToken string, err error)
}

// Manager reads and refreshes the stored session.
type Manager struct {
	File      string
	LockPath  string
	Refresher Refresher
	Now       func() time.Time
}

func dirOf(p string) string { return filepath.Dir(p) }

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Load reads the session file.
func (m *Manager) Load() (Session, error) {
	var s Session
	b, err := os.ReadFile(m.File)
	if errors.Is(err, fs.ErrNotExist) {
		return s, ErrNoSession
	}
	if err != nil {
		return s, fmt.Errorf("read session: %w", err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("parse session %s: %w", m.File, err)
	}
	if s.RefreshToken == "" && s.IDToken == "" {
		return s, ErrNoSession
	}
	return s, nil
}

// Save persists a session atomically (temp file, fsync, rename), mode 600.
func (m *Manager) Save(s Session) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(m.File, b)
}

// Delete removes the session file under the same lock a refresh holds, so a refresh that
// is in flight finishes first and cannot save a fresh session after Delete returns. The
// lock file stays: unlinking it would let a waiter lock a different inode.
func (m *Manager) Delete() error {
	lk, err := LockFile(m.LockPath)
	if err != nil {
		return fmt.Errorf("lock session: %w", err)
	}
	defer lk.Unlock()
	if err := os.Remove(m.File); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s Session) fresh(now time.Time) bool {
	return s.IDToken != "" && now.Add(RefreshMargin).Before(s.IDExpiry)
}

// IDToken returns a valid ID token and its expiry. With a fresh cached token it makes no
// network call and takes no lock.
func (m *Manager) IDToken(ctx context.Context) (string, time.Time, error) {
	s, err := m.Load()
	if err != nil {
		return "", time.Time{}, err
	}
	if s.fresh(m.now()) {
		return s.IDToken, s.IDExpiry, nil
	}
	cached := s
	s, err = m.refresh(ctx, "")
	if err != nil {
		// Palette could not be reached. A cached token that is still valid keeps kubectl
		// working until it expires. A rejected session or an expired token is an error.
		if !errors.Is(err, ErrSessionGone) && !errors.Is(err, ErrNoSession) &&
			cached.IDToken != "" && m.now().Add(StaleGrace).Before(cached.IDExpiry) {
			return cached.IDToken, cached.IDExpiry, nil
		}
		return "", time.Time{}, err
	}
	return s.IDToken, s.IDExpiry, nil
}

// APIToken returns the refresh token, which is the Palette API session. It does not call
// the network.
func (m *Manager) APIToken() (string, error) {
	s, err := m.Load()
	if err != nil {
		return "", err
	}
	if s.RefreshToken == "" {
		return "", ErrNoSession
	}
	return s.RefreshToken, nil
}

// RefreshAfterReject renews the session after the API rejected rejected. If another process
// already rotated the token, it returns the stored pair without spending it again.
func (m *Manager) RefreshAfterReject(ctx context.Context, rejected string) (string, error) {
	s, err := m.refresh(ctx, rejected)
	if err != nil {
		return "", err
	}
	return s.RefreshToken, nil
}

// refresh renews the session under the file lock. If staleRefresh is set and the stored
// refresh token differs, someone else already refreshed and nothing is spent.
func (m *Manager) refresh(ctx context.Context, staleRefresh string) (Session, error) {
	if m.Refresher == nil {
		return Session{}, ErrNoSession
	}
	lk, err := LockFile(m.LockPath)
	if err != nil {
		return Session{}, fmt.Errorf("lock session: %w", err)
	}
	defer lk.Unlock()

	s, err := m.Load()
	if err != nil {
		return s, err
	}
	if staleRefresh != "" {
		if s.RefreshToken != staleRefresh {
			return s, nil
		}
	} else if s.fresh(m.now()) {
		return s, nil
	}
	if s.RefreshToken == "" {
		return s, ErrNoSession
	}
	id, rt, err := m.Refresher.Refresh(ctx, s.RefreshToken)
	if err != nil {
		if errors.Is(err, ErrSessionGone) {
			return s, ErrSessionGone
		}
		return s, fmt.Errorf("refresh session: %w", err)
	}
	if rt == "" {
		rt = s.RefreshToken
	}
	exp, err := Expiry(id)
	if err != nil {
		return s, fmt.Errorf("refresh session: %w", err)
	}
	ns := Session{IDToken: id, RefreshToken: rt, IDExpiry: exp}
	// Persist before anything uses the new tokens: the old refresh token is spent.
	if err := m.Save(ns); err != nil {
		return s, fmt.Errorf("persist refreshed session (log in again if this repeats): %w", err)
	}
	return ns, nil
}

// Expiry reads the exp claim of a JWT without verifying it.
func Expiry(jwt string) (time.Time, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("token is not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, fmt.Errorf("decode token claims: %w", err)
	}
	var c struct {
		Exp float64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &c); err != nil || c.Exp == 0 {
		return time.Time{}, errors.New("token has no exp claim")
	}
	return time.Unix(int64(c.Exp), 0).UTC(), nil
}
