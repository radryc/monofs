package authz

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// PKCE holds a generated PKCE code verifier/challenge pair (RFC 7636).
type PKCE struct {
	Verifier  string
	Challenge string
	Method    string // always "S256"
}

// GeneratePKCE creates a cryptographically random code verifier and its S256
// challenge, used by the web UI Authorization Code + PKCE login (authz epic F2).
func GeneratePKCE() (PKCE, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return PKCE{}, fmt.Errorf("authz: generate pkce: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return PKCE{Verifier: verifier, Challenge: challenge, Method: "S256"}, nil
}

// AuthCodeURL builds the authorization endpoint URL for the PKCE flow.
func AuthCodeURL(authURL, clientID, redirectURI, state string, scopes []string, pkce PKCE) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", pkce.Challenge)
	q.Set("code_challenge_method", pkce.Method)
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	sep := "?"
	if strings.Contains(authURL, "?") {
		sep = "&"
	}
	return authURL + sep + q.Encode()
}

// PKCEExchangeConfig configures the authorization-code exchange.
type PKCEExchangeConfig struct {
	ClientID     string
	ClientSecret string // sent when non-empty (confidential clients)
	RedirectURI  string
	TokenURL     string
	HTTPClient   *http.Client
}

// ExchangeCode exchanges an authorization code + PKCE verifier for tokens.
func ExchangeCode(ctx context.Context, cfg PKCEExchangeConfig, code, verifier string) (*TokenResponse, error) {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("client_id", cfg.ClientID)
	if cfg.ClientSecret != "" {
		form.Set("client_secret", cfg.ClientSecret)
	}
	form.Set("redirect_uri", cfg.RedirectURI)
	form.Set("code_verifier", verifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("authz: build exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("authz: code exchange: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authz: code exchange failed with status %d", resp.StatusCode)
	}
	var tok TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return nil, fmt.Errorf("authz: decode token: %w", err)
	}
	tok.ObtainedAt = time.Now().Unix()
	return &tok, nil
}

// Session is a logged-in web session mapping a session id to an Identity.
type Session struct {
	ID        string
	Identity  Identity
	CreatedAt time.Time
	ExpiresAt time.Time
}

// SessionStore is a thread-safe in-memory web session store (authz epic F2).
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]Session
	ttl      time.Duration
	now      func() time.Time
}

// NewSessionStore builds a session store with the given TTL.
func NewSessionStore(ttl time.Duration) *SessionStore {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	return &SessionStore{
		sessions: make(map[string]Session),
		ttl:      ttl,
		now:      time.Now,
	}
}

// Create issues a new session for the identity and returns its opaque id.
func (s *SessionStore) Create(id Identity) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("authz: generate session id: %w", err)
	}
	sid := base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()
	s.mu.Lock()
	s.sessions[sid] = Session{ID: sid, Identity: id, CreatedAt: now, ExpiresAt: now.Add(s.ttl)}
	s.mu.Unlock()
	return sid, nil
}

// Get returns the identity for a session id, or ok=false when missing/expired.
func (s *SessionStore) Get(sid string) (Identity, bool) {
	s.mu.RLock()
	sess, ok := s.sessions[sid]
	s.mu.RUnlock()
	if !ok {
		return Identity{}, false
	}
	if s.now().After(sess.ExpiresAt) {
		s.Delete(sid)
		return Identity{}, false
	}
	return sess.Identity, true
}

// Delete removes a session (logout).
func (s *SessionStore) Delete(sid string) {
	s.mu.Lock()
	delete(s.sessions, sid)
	s.mu.Unlock()
}

// Snapshot returns a copy of all active (non-expired) sessions.
func (s *SessionStore) Snapshot() map[string]Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Session, len(s.sessions))
	now := s.now()
	for sid, sess := range s.sessions {
		if now.Before(sess.ExpiresAt) {
			out[sid] = sess
		}
	}
	return out
}

// Restore imports sessions. Only sessions that have not yet expired are kept.
func (s *SessionStore) Restore(sessions map[string]Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for sid, sess := range sessions {
		if now.Before(sess.ExpiresAt) {
			s.sessions[sid] = sess
		}
	}
}

// PersistentSessionStore wraps a SessionStore with file-backed persistence so
// browser sessions survive process restarts. It writes a JSON snapshot to disk
// on every Create/Delete and restores them on startup.
type PersistentSessionStore struct {
	store *SessionStore
	dir   string
	mu    sync.Mutex
}

// NewPersistentSessionStore creates (or restores) a file-backed session store.
func NewPersistentSessionStore(dir string, ttl time.Duration) (*PersistentSessionStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("authz: create session dir: %w", err)
	}
	store := NewSessionStore(ttl)
	ps := &PersistentSessionStore{store: store, dir: dir}
	ps.restoreFromDisk()
	return ps, nil
}

// SessionStore returns the underlying in-memory SessionStore.
func (ps *PersistentSessionStore) SessionStore() *SessionStore {
	return ps.store
}

func (ps *PersistentSessionStore) sessionsPath() string {
	return filepath.Join(ps.dir, "sessions.json")
}

func (ps *PersistentSessionStore) persistToDisk() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	state := ps.store.Snapshot()
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	tmp := filepath.Join(ps.dir, ".sessions.tmp")
	dst := ps.sessionsPath()
	os.WriteFile(tmp, data, 0600)
	os.Rename(tmp, dst)
}

func (ps *PersistentSessionStore) restoreFromDisk() {
	data, err := os.ReadFile(ps.sessionsPath())
	if err != nil {
		return
	}
	var state map[string]Session
	if err := json.Unmarshal(data, &state); err != nil {
		return
	}
	ps.store.Restore(state)
}

// Create issues a new session and persists.
func (ps *PersistentSessionStore) Create(id Identity) (string, error) {
	sid, err := ps.store.Create(id)
	if err != nil {
		return sid, err
	}
	ps.persistToDisk()
	return sid, nil
}

// Delete removes a session and persists.
func (ps *PersistentSessionStore) Delete(sid string) {
	ps.store.Delete(sid)
	ps.persistToDisk()
}

// Get looks up a session.
func (ps *PersistentSessionStore) Get(sid string) (Identity, bool) {
	return ps.store.Get(sid)
}
