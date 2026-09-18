package hostapi

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MockHost is an in-memory Host used by tests and by the development harness
// (cmd/plugin without the cshared tag).
//
// It is deliberately concurrent-safe: the probe scheduler exercises it from
// several goroutines at once, and a data race here would mask a real one.
type MockHost struct {
	mu       sync.RWMutex
	accounts []Account
	creds    map[string]Credential
	logs     []LogEntry

	// CredentialErr, when set for an authIndex, is returned by GetCredential.
	credentialErr map[string]error
	// ListErr, when non-nil, is returned by ListAccounts.
	ListErr error
}

// LogEntry is a captured log line.
type LogEntry struct {
	Level  LogLevel
	Msg    string
	Fields map[string]any
	At     time.Time
}

// NewMockHost builds a MockHost with a deterministic Codex account pool.
func NewMockHost(accounts ...Account) *MockHost {
	h := &MockHost{
		creds:         map[string]Credential{},
		credentialErr: map[string]error{},
	}
	for _, a := range accounts {
		h.AddAccount(a)
	}
	return h
}

// AddAccount registers an account and a matching credential.
func (h *MockHost) AddAccount(a Account) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if a.Provider == "" {
		a.Provider = ProviderCodex
	}
	if a.Status == "" {
		a.Status = AccountStatusAvailable
	}
	h.accounts = append(h.accounts, a)
	h.creds[a.AuthIndex] = Credential{
		AccessToken: "mock-access-token-" + a.AuthIndex,
		ExpiresAt:   time.Now().Add(time.Hour),
		Raw:         map[string]any{"access_token": "mock-access-token-" + a.AuthIndex},
	}
}

// SetCredential replaces the credential returned for an authIndex.
func (h *MockHost) SetCredential(authIndex string, c Credential) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.creds[authIndex] = c
}

// SetCredentialError makes GetCredential fail for an authIndex.
func (h *MockHost) SetCredentialError(authIndex string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.credentialErr[authIndex] = err
}

// SetAccounts replaces the whole pool.
func (h *MockHost) SetAccounts(accounts []Account) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.accounts = accounts
}

// Logs returns a copy of everything logged so far.
func (h *MockHost) Logs() []LogEntry {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]LogEntry, len(h.logs))
	copy(out, h.logs)
	return out
}

// ListAccounts implements Host.
func (h *MockHost) ListAccounts(ctx context.Context) ([]Account, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.ListErr != nil {
		return nil, h.ListErr
	}
	out := make([]Account, len(h.accounts))
	copy(out, h.accounts)
	sort.SliceStable(out, func(i, j int) bool { return out[i].AuthIndex < out[j].AuthIndex })
	return out, nil
}

// GetCredential implements Host.
func (h *MockHost) GetCredential(ctx context.Context, authIndex string) (Credential, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if err, ok := h.credentialErr[authIndex]; ok {
		return Credential{}, err
	}
	c, ok := h.creds[authIndex]
	if !ok {
		return Credential{}, fmt.Errorf("hostapi: unknown auth_index %q", authIndex)
	}
	return c, nil
}

// Log implements Host.
func (h *MockHost) Log(level LogLevel, msg string, fields map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logs = append(h.logs, LogEntry{Level: level, Msg: msg, Fields: fields, At: time.Now()})
}
