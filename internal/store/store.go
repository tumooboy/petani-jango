// Package store persists the account pool and keeps it safe on disk (0600,
// atomic rename, append-only JSONL audit trail).
package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Account is the durable record of one Jango account.
type Account struct {
	ID            string    `json:"id"`
	Email         string    `json:"email"`
	Password      string    `json:"password"`
	Name          string    `json:"name"`
	InboxPassword string    `json:"inboxPassword,omitempty"`
	AccessToken   string    `json:"accessToken"`
	TokenExpiry   time.Time `json:"tokenExpiry"`
	RefreshCookie string    `json:"refreshCookie"`
	CookieExpiry  time.Time `json:"cookieExpiry"`
	FirebaseUID   string    `json:"firebaseUid,omitempty"`
	Credits       int       `json:"credits"`
	Verified      bool      `json:"verified"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
	Error         string    `json:"error,omitempty"`
}

// Store is a JSON account file plus a JSONL audit log in the same directory.
type Store struct {
	mu    sync.Mutex
	path  string
	items []*Account
}

// Open loads (or initialises) the store at path.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return s, nil
	}
	var doc struct {
		Accounts []*Account `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("store: %s is not valid JSON: %w", path, err)
	}
	s.items = doc.Accounts
	return s, nil
}

// Accounts returns a copy of the slice (the pointers are shared).
func (s *Store) Accounts() []*Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Account, len(s.items))
	copy(out, s.items)
	return out
}

// Upsert inserts or replaces an account by id (or email when id is empty).
func (s *Store) Upsert(a *Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.UpdatedAt = time.Now().UTC()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = a.UpdatedAt
	}
	key := a.ID
	if key == "" {
		key = a.Email
		a.ID = key
	}
	for i, x := range s.items {
		if x.ID == key || (a.Email != "" && x.Email == a.Email) {
			s.items[i] = a
			return s.flushLocked()
		}
	}
	s.items = append(s.items, a)
	return s.flushLocked()
}

// Log appends one audit record to accounts.jsonl.
func (s *Store) Log(event string, m map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(filepath.Dir(s.path), "accounts.jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	rec := map[string]any{"at": time.Now().UTC().Format(time.RFC3339), "event": event}
	for k, v := range m {
		rec[k] = v
	}
	b, _ := json.Marshal(rec)
	_, err = f.Write(append(b, '\n'))
	return err
}

// ReadLog returns the audit trail (last n records when n > 0).
func (s *Store) ReadLog(n int) ([]map[string]any, error) {
	f, err := os.Open(filepath.Join(filepath.Dir(s.path), "accounts.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, sc.Err()
}

func (s *Store) flushLocked() error {
	b, err := json.MarshalIndent(map[string]any{
		"version": 1, "updatedAt": time.Now().UTC(), "accounts": s.items}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
