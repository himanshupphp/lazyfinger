// Package setup serves the visual configurator for `lazy setup`.
package setup

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/himanshupphp/lazyfinger/internal/config"
	"github.com/himanshupphp/lazyfinger/internal/provider"
	"github.com/himanshupphp/lazyfinger/internal/secrets"
	"github.com/himanshupphp/lazyfinger/internal/ui"
)

//go:embed index.html
var page []byte

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type providerDTO struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	BaseURL   string `json:"baseUrl"`
	Model     string `json:"model"`
	APIKeyEnv string `json:"apiKeyEnv"`
	MaxTokens int    `json:"maxTokens"`
	APIKey    string `json:"apiKey,omitempty"` // only ever sent by the browser, never back
	KeySource string `json:"keySource"`        // "env", "keychain" or ""
}

type stateDTO struct {
	Path        string        `json:"path"`
	Exists      bool          `json:"exists"`
	Default     string        `json:"default"`
	Mode        config.Mode   `json:"mode"`
	MaxAttempts int           `json:"maxAttempts"`
	Providers   []providerDTO `json:"providers"`
}

func (p providerDTO) config() config.Provider {
	t := p.Type
	if t == "" {
		t = "openai"
	}
	return config.Provider{
		Type:      t,
		BaseURL:   strings.TrimRight(strings.TrimSpace(p.BaseURL), "/"),
		Model:     strings.TrimSpace(p.Model),
		APIKeyEnv: strings.TrimSpace(p.APIKeyEnv),
		MaxTokens: p.MaxTokens,
	}
}

// key prefers a key typed into the form, then any stored one.
func (p providerDTO) key() string {
	if k := strings.TrimSpace(p.APIKey); k != "" {
		return k
	}
	k, _ := provider.ResolveKey(p.Name, p.config())
	return k
}

type server struct {
	addr, token string
	done        chan struct{}
	once        sync.Once
	mu          sync.Mutex
	savedPath   string
}

func Run() int {
	ln, err := net.Listen("tcp", "127.0.0.1:0") // this machine only, random free port
	if err != nil {
		ui.Error("Could not start the configurator: " + err.Error())
		return 1
	}
	s := &server{addr: ln.Addr().String(), token: randomToken(), done: make(chan struct{})}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/api/state", s.api(s.state))
	mux.HandleFunc("/api/detect", s.api(s.detect))
	mux.HandleFunc("/api/models", s.api(s.models))
	mux.HandleFunc("/api/test", s.api(s.test))
	mux.HandleFunc("/api/save", s.api(s.save))
	mux.HandleFunc("/api/close", s.api(s.finish))
	srv := &http.Server{Handler: s.guard(mux), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()

	link := fmt.Sprintf("http://%s/?t=%s", s.addr, s.token)
	ui.Header("Setup")
	ui.Muted("Opening the configurator in your browser. If it doesn't open, visit:")
	fmt.Println("  " + link)
	if err := openBrowser(link); err != nil {
		ui.Muted("(Couldn't open a browser automatically.)")
	}
	ui.Muted("Press Ctrl-C to quit without saving.")

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)
	select {
	case <-s.done:
	case <-sigs:
		fmt.Println()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)

	s.mu.Lock()
	path := s.savedPath
	s.mu.Unlock()
	if path == "" {
		ui.Muted("Closed without saving.")
		return 0
	}
	ui.Success("Saved " + path)
	return 0
}

// guard blocks everything except this page in your browser: the Host check
// stops DNS-rebinding attacks and the token stops other local programs and sites.
func (s *server) guard(next http.Handler) http.Handler {
	_, port, _ := net.SplitHostPort(s.addr)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != s.addr && r.Host != "localhost:"+port {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		tok := r.Header.Get("X-Lazy-Token")
		if r.URL.Path == "/" {
			tok = r.URL.Query().Get("t")
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

func (s *server) api(fn func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data, err := fn(r)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(data)
	}
}

func (s *server) state(*http.Request) (any, error) {
	cfg, err := config.LoadUser()
	if err != nil {
		return nil, err
	}
	path, _ := config.Path()
	out := stateDTO{
		Path: path, Exists: config.Exists(),
		Default: cfg.Default, Mode: cfg.Mode, MaxAttempts: cfg.MaxAttempts,
		Providers: []providerDTO{},
	}
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		pc := cfg.Providers[n]
		_, src := provider.ResolveKey(n, pc)
		t := pc.Type
		if t == "" {
			t = "openai"
		}
		out.Providers = append(out.Providers, providerDTO{
			Name: n, Type: t, BaseURL: pc.BaseURL, Model: pc.Model,
			APIKeyEnv: pc.APIKeyEnv, MaxTokens: pc.MaxTokens, KeySource: src,
		})
	}
	return out, nil
}

func (s *server) detect(r *http.Request) (any, error) {
	type found struct {
		ID      string   `json:"id"`
		Label   string   `json:"label"`
		BaseURL string   `json:"baseUrl"`
		Models  []string `json:"models"`
	}
	candidates := []found{
		{ID: "ollama", Label: "Ollama", BaseURL: "http://localhost:11434/v1"},
		{ID: "lmstudio", Label: "LM Studio", BaseURL: "http://localhost:1234/v1"},
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	ok := make([]bool, len(candidates))
	var wg sync.WaitGroup
	for i := range candidates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			models, err := provider.ListModels(ctx, config.Provider{Type: "openai", BaseURL: candidates[i].BaseURL}, "")
			if err == nil {
				candidates[i].Models, ok[i] = models, true
			}
		}()
	}
	wg.Wait()
	out := []found{}
	for i, c := range candidates {
		if ok[i] {
			if c.Models == nil {
				c.Models = []string{}
			}
			out = append(out, c)
		}
	}
	return map[string]any{"found": out}, nil
}

func (s *server) models(r *http.Request) (any, error) {
	p, err := readProvider(r)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	models, err := provider.ListModels(ctx, p.config(), p.key())
	if err != nil {
		return nil, friendly(err)
	}
	if models == nil {
		models = []string{}
	}
	return map[string]any{"models": models}, nil
}

func (s *server) test(r *http.Request) (any, error) {
	p, err := readProvider(r)
	if err != nil {
		return nil, err
	}
	prov, err := provider.NewWithKey(p.Name, p.config(), p.key())
	if err != nil {
		return nil, err
	}
	// Generous timeout: local models may need to load into memory first.
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	start := time.Now()
	reply, err := prov.Complete(ctx, "You are a connection test. Follow the instruction exactly.",
		[]provider.Message{{Role: "user", Content: "Reply with just the word OK."}})
	if err != nil {
		return nil, friendly(err)
	}
	reply = strings.TrimSpace(reply)
	if len(reply) > 80 {
		reply = reply[:80] + "..."
	}
	return map[string]any{"ms": time.Since(start).Milliseconds(), "reply": reply}, nil
}

func (s *server) save(r *http.Request) (any, error) {
	if r.Method != http.MethodPost {
		return nil, errors.New("use POST")
	}
	var in stateDTO
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		return nil, err
	}
	if len(in.Providers) == 0 {
		return nil, errors.New("add at least one model provider")
	}
	if in.Mode != config.Analyze && in.Mode != config.Autofix {
		return nil, fmt.Errorf("unknown mode %q", in.Mode)
	}
	if in.MaxAttempts < 1 || in.MaxAttempts > 10 {
		in.MaxAttempts = 3
	}

	old, _ := config.LoadUser()
	cfg := config.Config{Default: in.Default, Mode: in.Mode, MaxAttempts: in.MaxAttempts, Providers: map[string]config.Provider{}}
	for _, p := range in.Providers {
		if !nameRe.MatchString(p.Name) {
			return nil, fmt.Errorf("%q isn't a valid provider name", p.Name)
		}
		if _, dup := cfg.Providers[p.Name]; dup {
			return nil, fmt.Errorf("two providers are named %q", p.Name)
		}
		pc := p.config()
		switch {
		case pc.Type != "openai" && pc.Type != "anthropic":
			return nil, fmt.Errorf("%q has an unknown type %q", p.Name, pc.Type)
		case pc.Model == "":
			return nil, fmt.Errorf("choose a model for %q", p.Name)
		case pc.Type == "openai" && pc.BaseURL == "":
			return nil, fmt.Errorf("%q needs a server URL", p.Name)
		}
		if prev, ok := old.Providers[p.Name]; ok {
			pc.Temperature = prev.Temperature // keep settings the UI doesn't show
		}
		cfg.Providers[p.Name] = pc
	}
	for _, n := range old.Fallback { // keep the fallback order, minus removed providers
		if _, ok := cfg.Providers[n]; ok {
			cfg.Fallback = append(cfg.Fallback, n)
		}
	}
	if _, ok := cfg.Providers[cfg.Default]; !ok {
		cfg.Default = in.Providers[0].Name
	}

	// Keys go to the system keychain, never the config file.
	for _, p := range in.Providers {
		if k := strings.TrimSpace(p.APIKey); k != "" {
			if err := secrets.Set(p.Name, k); err != nil {
				return nil, fmt.Errorf("couldn't save the key for %q to your system keychain (%v). Use Advanced → environment variable instead", p.Name, err)
			}
		}
	}
	for name := range old.Providers {
		if _, kept := cfg.Providers[name]; !kept {
			_ = secrets.Delete(name)
		}
	}

	path, err := config.Save(cfg)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.savedPath = path
	s.mu.Unlock()
	return map[string]string{"path": path}, nil
}

func (s *server) finish(*http.Request) (any, error) {
	go func() {
		time.Sleep(300 * time.Millisecond) // let the response reach the browser
		s.once.Do(func() { close(s.done) })
	}()
	return map[string]bool{"ok": true}, nil
}

func readProvider(r *http.Request) (providerDTO, error) {
	var p providerDTO
	if r.Method != http.MethodPost {
		return p, errors.New("use POST")
	}
	err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p)
	return p, err
}

func friendly(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "actively refused"):
		return errors.New("nothing is answering at that URL. Is the server running?")
	case strings.Contains(msg, "HTTP 401"), strings.Contains(msg, "HTTP 403"):
		return errors.New("the server rejected the API key. Check that it's correct")
	case strings.Contains(msg, "no such host"):
		return errors.New("couldn't find that server. Check the URL")
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("the server took too long to respond")
	}
	return err
}

func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func openBrowser(link string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", link).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", link).Start()
	default:
		return exec.Command("xdg-open", link).Start()
	}
}
