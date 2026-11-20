package session

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func jwt(exp time.Time, tag string) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	p := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"tag":%q}`, exp.Unix(), tag)))
	return h + "." + p + ".sig"
}

type fakeRefresher struct {
	calls   atomic.Int32
	mu      sync.Mutex
	current string // the only refresh token the "server" still accepts
	n       int
	now     time.Time
	err     error
}

func (f *fakeRefresher) Refresh(_ context.Context, rt string) (string, string, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", "", f.err
	}
	if rt != f.current {
		return "", "", ErrSessionGone
	}
	f.n++
	f.current = fmt.Sprintf("rt-%d", f.n)
	time.Sleep(20 * time.Millisecond)
	return jwt(f.now.Add(time.Hour), fmt.Sprint(f.n)), f.current, nil
}

func newMgr(t *testing.T, r Refresher, now time.Time) *Manager {
	dir := t.TempDir()
	return &Manager{File: filepath.Join(dir, "s.json"), LockPath: filepath.Join(dir, "s.lock"), Refresher: r, Now: func() time.Time { return now }}
}

func TestFreshTokenNoRefresh(t *testing.T) {
	now := time.Now()
	fr := &fakeRefresher{now: now}
	m := newMgr(t, fr, now)
	id := jwt(now.Add(30*time.Minute), "a")
	if err := m.Save(Session{IDToken: id, RefreshToken: "rt-0", IDExpiry: now.Add(30 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	got, _, err := m.IDToken(context.Background())
	if err != nil || got != id {
		t.Fatalf("got %q err %v", got, err)
	}
	if fr.calls.Load() != 0 {
		t.Fatal("fresh token must not refresh")
	}
}

func TestRefreshRotationPersisted(t *testing.T) {
	now := time.Now()
	fr := &fakeRefresher{now: now, current: "rt-0"}
	m := newMgr(t, fr, now)
	// within the 60 s margin
	m.Save(Session{IDToken: jwt(now.Add(30*time.Second), "old"), RefreshToken: "rt-0", IDExpiry: now.Add(30 * time.Second)})
	id, exp, err := m.IDToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if exp.Before(now.Add(50 * time.Minute)) {
		t.Fatalf("expiry not advanced: %v", exp)
	}
	s, _ := m.Load()
	if s.IDToken != id || s.RefreshToken != "rt-1" {
		t.Fatalf("rotated pair not persisted: %+v", s)
	}
	fi, _ := os.Stat(m.File)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

func TestConcurrentRefreshSpendsTokenOnce(t *testing.T) {
	now := time.Now()
	fr := &fakeRefresher{now: now, current: "rt-0"}
	m := newMgr(t, fr, now)
	m.Save(Session{IDToken: jwt(now.Add(-time.Minute), "old"), RefreshToken: "rt-0", IDExpiry: now.Add(-time.Minute)})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// separate managers share only the files, like separate processes
			mm := &Manager{File: m.File, LockPath: m.LockPath, Refresher: fr, Now: m.Now}
			_, _, err := mm.IDToken(context.Background())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent caller failed: %v", err)
		}
	}
	if fr.calls.Load() != 1 {
		t.Fatalf("refresh token spent %d times, want 1", fr.calls.Load())
	}
}

func TestSessionGone(t *testing.T) {
	now := time.Now()
	fr := &fakeRefresher{now: now, current: "other"}
	m := newMgr(t, fr, now)
	m.Save(Session{IDToken: jwt(now.Add(-time.Minute), "old"), RefreshToken: "rt-0", IDExpiry: now.Add(-time.Minute)})
	if _, _, err := m.IDToken(context.Background()); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("err = %v", err)
	}
	// a rejected refresh must not destroy the stored session
	if s, err := m.Load(); err != nil || s.RefreshToken != "rt-0" {
		t.Fatalf("session was altered: %+v %v", s, err)
	}
}

func TestNoSession(t *testing.T) {
	m := newMgr(t, &fakeRefresher{}, time.Now())
	if _, _, err := m.IDToken(context.Background()); !errors.Is(err, ErrNoSession) {
		t.Fatalf("err = %v", err)
	}
}

func TestRefreshAfterRejectSkipsWhenAlreadyRotated(t *testing.T) {
	now := time.Now()
	fr := &fakeRefresher{now: now, current: "rt-9"}
	m := newMgr(t, fr, now)
	m.Save(Session{IDToken: jwt(now.Add(time.Hour), "a"), RefreshToken: "rt-9", IDExpiry: now.Add(time.Hour)})
	got, err := m.RefreshAfterReject(context.Background(), "rt-8")
	if err != nil || got != "rt-9" || fr.calls.Load() != 0 {
		t.Fatalf("got %q err %v calls %d", got, err, fr.calls.Load())
	}
	got, err = m.RefreshAfterReject(context.Background(), "rt-9")
	if err != nil || got != "rt-10" && got != "rt-1" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestExpiry(t *testing.T) {
	for _, tc := range []struct {
		name, tok string
		ok        bool
	}{{"valid", jwt(time.Now().Add(time.Hour), "x"), true}, {"not jwt", "abc", false}, {"bad b64", "a.!!.c", false}} {
		_, err := Expiry(tc.tok)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err %v", tc.name, err)
		}
	}
}

// Logout must wait for an in-flight refresh and win: the refresh may not save a session
// after Delete has returned.
func TestDeleteWaitsForRefreshAndWins(t *testing.T) {
	now := time.Now()
	gate := make(chan struct{})
	started := make(chan struct{})
	r := &blockingRefresher{now: now, gate: gate, started: started}
	m := newMgr(t, r, now)
	if err := m.Save(Session{IDToken: jwt(now.Add(-time.Hour), "old"), RefreshToken: "rt-0", IDExpiry: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	refreshDone := make(chan error, 1)
	go func() { _, _, err := m.IDToken(context.Background()); refreshDone <- err }()
	<-started
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- m.Delete() }()
	select {
	case <-deleteDone:
		t.Fatal("Delete returned while a refresh held the lock")
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	if err := <-refreshDone; err != nil {
		t.Fatal(err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatal(err)
	}
	if _, err := m.Load(); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a refresh resurrected the session after logout: %v", err)
	}
}

// A refresh that starts after Delete must find no session and save nothing.
func TestRefreshAfterDeleteSavesNothing(t *testing.T) {
	now := time.Now()
	fr := &fakeRefresher{now: now, current: "rt-0"}
	m := newMgr(t, fr, now)
	m.Save(Session{IDToken: jwt(now.Add(-time.Hour), "old"), RefreshToken: "rt-0", IDExpiry: now.Add(-time.Hour)})
	if err := m.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.IDToken(context.Background()); !errors.Is(err, ErrNoSession) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(m.File); err == nil {
		t.Fatal("session file recreated")
	}
}

type blockingRefresher struct {
	now           time.Time
	gate, started chan struct{}
}

func (b *blockingRefresher) Refresh(_ context.Context, _ string) (string, string, error) {
	close(b.started)
	<-b.gate
	return jwt(b.now.Add(time.Hour), "new"), "rt-1", nil
}

func staleSetup(t *testing.T, left time.Duration, err error) (*Manager, *fakeRefresher, string) {
	now := time.Now()
	fr := &fakeRefresher{now: now, current: "rt-0", err: err}
	m := newMgr(t, fr, now)
	id := jwt(now.Add(left), "old")
	if e := m.Save(Session{IDToken: id, RefreshToken: "rt-0", IDExpiry: now.Add(left)}); e != nil {
		t.Fatal(e)
	}
	return m, fr, id
}

func TestRefreshNetworkErrorInsideMarginUsesCachedToken(t *testing.T) {
	m, fr, id := staleSetup(t, 30*time.Second, errors.New("dial tcp: connection refused"))
	got, exp, err := m.IDToken(context.Background())
	if err != nil || got != id {
		t.Fatalf("got %q err %v", got, err)
	}
	if exp.IsZero() || fr.calls.Load() != 1 {
		t.Fatalf("exp %v calls %d", exp, fr.calls.Load())
	}
}

func TestRefreshNetworkErrorAfterExpiryFails(t *testing.T) {
	m, _, _ := staleSetup(t, -time.Minute, errors.New("dial tcp: connection refused"))
	if _, _, err := m.IDToken(context.Background()); err == nil {
		t.Fatal("expired token must return the error")
	}
}

func TestRefreshNetworkErrorInsideSafetyMarginFails(t *testing.T) {
	m, _, _ := staleSetup(t, 5*time.Second, errors.New("timeout"))
	if _, _, err := m.IDToken(context.Background()); err == nil {
		t.Fatal("token inside the 10 s safety margin must return the error")
	}
}

func TestRefreshSessionGoneInsideMarginFails(t *testing.T) {
	m, _, _ := staleSetup(t, 30*time.Second, ErrSessionGone)
	_, _, err := m.IDToken(context.Background())
	if !errors.Is(err, ErrSessionGone) {
		t.Fatalf("got %v", err)
	}
}

func TestSuccessfulRefreshStillReplacesToken(t *testing.T) {
	m, _, old := staleSetup(t, 30*time.Second, nil)
	got, _, err := m.IDToken(context.Background())
	if err != nil || got == old {
		t.Fatalf("token not replaced: %v", err)
	}
}

func TestRejectedNamesTheSpentRefreshToken(t *testing.T) {
	m, _, _ := staleSetup(t, -time.Minute, ErrSessionGone)
	_, _, err := m.IDToken(context.Background())
	rt, ok := Rejected(err)
	if !errors.Is(err, ErrSessionGone) || !ok || rt == "" {
		t.Fatalf("%v %q %v", err, rt, ok)
	}
	if s, _ := m.Load(); s.RefreshToken != rt {
		t.Fatalf("rejected %q, stored %q", rt, s.RefreshToken)
	}
}
