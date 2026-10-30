package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/himanshupphp/lazyfinger/internal/config"
)

// HTTPError is a non-2xx response from a provider.
type HTTPError struct {
	Status     int
	Body       string
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string { return "HTTP " + itoa(e.Status) + ": " + e.Body }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// Classify reports whether an error means "this provider can't serve us right
// now", with a plain reason and how long to pause it. ok is false for errors
// that another provider wouldn't fix (like a malformed request).
func Classify(err error) (reason string, pause time.Duration, ok bool) {
	if errors.Is(err, context.Canceled) {
		return "", 0, false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		body := strings.ToLower(he.Body)
		switch {
		case he.Status == 402 || strings.Contains(body, "insufficient_quota") || strings.Contains(body, "credit balance") ||
			strings.Contains(body, "billing") || strings.Contains(body, "quota"):
			return "ran out of credits or quota", time.Hour, true
		case he.Status == 429:
			wait := he.RetryAfter
			if wait <= 0 {
				wait = time.Minute
			}
			return "is rate limited", wait, true
		case he.Status == 529 || (he.Status == 503 && strings.Contains(body, "overload")):
			return "is overloaded", time.Minute, true
		case he.Status == 401 || he.Status == 403:
			return "rejected the API key", 10 * time.Minute, true
		case he.Status == 404 && strings.Contains(body, "model"):
			return "doesn't have that model", 0, true
		}
		return "", 0, false
	}
	var ne net.Error
	var oe *net.OpError
	if errors.As(err, &oe) || errors.As(err, &ne) || strings.Contains(err.Error(), "connection refused") {
		return "isn't reachable", 0, true
	}
	return "", 0, false
}

type Cooldown struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason"`
}

// State remembers paused providers across runs, in the user's config folder.
type State struct {
	mu        sync.Mutex
	path      string
	Cooldowns map[string]Cooldown `json:"cooldowns"`
}

func LoadState() *State {
	s := &State{Cooldowns: map[string]Cooldown{}}
	dir, err := config.Dir()
	if err != nil {
		return s
	}
	s.path = filepath.Join(dir, "state.json")
	if data, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(data, s)
	}
	if s.Cooldowns == nil {
		s.Cooldowns = map[string]Cooldown{}
	}
	return s
}

func (s *State) Cooling(name string) (Cooldown, bool) {
	if s == nil {
		return Cooldown{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cd, ok := s.Cooldowns[name]
	return cd, ok && time.Now().Before(cd.Until)
}

// Active returns the providers that are currently paused.
func (s *State) Active() map[string]Cooldown {
	out := map[string]Cooldown{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.Cooldowns {
		if time.Now().Before(v.Until) {
			out[k] = v
		}
	}
	return out
}

func (s *State) Set(name, reason string, until time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Cooldowns[name] = Cooldown{Until: until, Reason: reason}
	s.save()
}

func (s *State) Clear(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.Cooldowns[name]; !ok {
		return
	}
	delete(s.Cooldowns, name)
	s.save()
}

func (s *State) save() {
	if s.path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	data, err := json.MarshalIndent(s, "", "  ")
	if err == nil {
		_ = os.WriteFile(s.path, data, 0o600)
	}
}
