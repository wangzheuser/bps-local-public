// Package admin serves the management Web UI and REST API behind an
// administrator session. Sessions live in memory only; a restart logs
// everyone out by design.
package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	sessionCookieName = "bps_admin_session"
	maxLoginFailures  = 10
	loginLockWindow   = 5 * time.Minute
)

var (
	errLocked          = errors.New("too many failed attempts; try again later")
	errInvalidPassword = errors.New("invalid password")
)

// SessionManager keeps server-side sessions and per-IP login throttling.
type SessionManager struct {
	mu          sync.Mutex
	sessions    map[string]*adminSession // key: sha256 hex of cookie token
	ttl         time.Duration
	cookieSecure bool
	password    string
	failures    map[string]*loginFailure
	now         func() time.Time
}

type adminSession struct {
	csrf      string
	expiresAt time.Time
}

type loginFailure struct {
	count    int
	lockedUntil time.Time
}

// NewSessionManager builds the manager. password comes from the secret file.
func NewSessionManager(password string, ttl time.Duration, cookieSecure bool) *SessionManager {
	return &SessionManager{
		sessions:     make(map[string]*adminSession),
		ttl:          ttl,
		cookieSecure: cookieSecure,
		password:     password,
		failures:     make(map[string]*loginFailure),
		now:          time.Now,
	}
}

func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func (m *SessionManager) hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Login validates the password and creates a session. It returns the cookie
// token and the CSRF token for the session.
func (m *SessionManager) Login(password, remoteIP string) (cookieToken, csrf string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if f := m.failures[remoteIP]; f != nil && f.lockedUntil.After(now) {
		return "", "", errLocked
	}
	if subtle.ConstantTimeCompare([]byte(password), []byte(m.password)) != 1 {
		f := m.failures[remoteIP]
		if f == nil || !f.lockedUntil.After(now) {
			f = &loginFailure{}
			m.failures[remoteIP] = f
		}
		f.count++
		if f.count >= maxLoginFailures {
			f.lockedUntil = now.Add(loginLockWindow)
			f.count = 0
		}
		return "", "", errInvalidPassword
	}
	delete(m.failures, remoteIP)
	token, err := randomToken()
	if err != nil {
		return "", "", err
	}
	csrfToken, err := randomToken()
	if err != nil {
		return "", "", err
	}
	m.sessions[m.hash(token)] = &adminSession{csrf: csrfToken, expiresAt: now.Add(m.ttl)}
	m.pruneLocked(now)
	return token, csrfToken, nil
}

// Validate resolves the session from the request cookie, sliding the expiry.
func (m *SessionManager) Validate(r *http.Request) (*adminSession, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[m.hash(c.Value)]
	if !ok {
		return nil, false
	}
	now := m.now()
	if !s.expiresAt.After(now) {
		delete(m.sessions, m.hash(c.Value))
		return nil, false
	}
	s.expiresAt = now.Add(m.ttl)
	return s, true
}

// CSRF validates the X-CSRF-Token header against the session and checks Origin.
func (m *SessionManager) CSRF(r *http.Request, s *adminSession) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			return false
		}
	}
	token := r.Header.Get("X-CSRF-Token")
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.csrf)) == 1
}

// Logout removes the session bound to the request cookie.
func (m *SessionManager) Logout(r *http.Request) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, m.hash(c.Value))
}

func (m *SessionManager) pruneLocked(now time.Time) {
	for k, s := range m.sessions {
		if !s.expiresAt.After(now) {
			delete(m.sessions, k)
		}
	}
}
