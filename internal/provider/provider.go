package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/himanshupphp/lazyfinger/internal/config"
	"github.com/himanshupphp/lazyfinger/internal/secrets"
)

type Message struct {
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
}

type Provider interface {
	Complete(ctx context.Context, system string, msgs []Message) (string, error)
	Name() string
	Remote() bool // true if code leaves this machine
}

// ResolveKey finds a provider's API key: its environment variable first, then
// the system keychain (where `lazy setup` stores keys). source is "env",
// "keychain", or "" when no key was found.
func ResolveKey(name string, c config.Provider) (key, source string) {
	if c.APIKeyEnv != "" {
		if v := os.Getenv(c.APIKeyEnv); v != "" {
			return v, "env"
		}
	}
	if v, err := secrets.Get(name); err == nil && v != "" {
		return v, "keychain"
	}
	return "", ""
}

func New(name string, c config.Provider) (Provider, error) {
	key, _ := ResolveKey(name, c)
	if key == "" && c.APIKeyEnv != "" {
		return nil, fmt.Errorf("provider %q: environment variable %s is not set (or run `lazy setup` to store a key)", name, c.APIKeyEnv)
	}
	return NewWithKey(name, c, key)
}

func NewWithKey(name string, c config.Provider, key string) (Provider, error) {
	if c.Model == "" {
		return nil, fmt.Errorf("provider %q: no model chosen (run `lazy setup`)", name)
	}
	switch c.Type {
	case "openai", "":
		if c.BaseURL == "" {
			return nil, fmt.Errorf("provider %q: base_url is required", name)
		}
		return &OpenAI{name: name, cfg: c, key: key}, nil
	case "anthropic":
		if key == "" {
			return nil, fmt.Errorf("provider %q needs an API key (run `lazy setup` to add one)", name)
		}
		return &Anthropic{name: name, cfg: c, key: key}, nil
	}
	return nil, fmt.Errorf("provider %q: unknown type %q", name, c.Type)
}

// ListModels asks a provider which models it offers.
func ListModels(ctx context.Context, c config.Provider, key string) ([]string, error) {
	var endpoint string
	headers := map[string]string{}
	if c.Type == "anthropic" {
		if key == "" {
			return nil, errors.New("an API key is needed to list Claude models")
		}
		base := c.BaseURL
		if base == "" {
			base = "https://api.anthropic.com"
		}
		endpoint = strings.TrimRight(base, "/") + "/v1/models?limit=100"
		headers["x-api-key"] = key
		headers["anthropic-version"] = "2023-06-01"
	} else {
		if c.BaseURL == "" {
			return nil, errors.New("a server URL is required")
		}
		endpoint = strings.TrimRight(c.BaseURL, "/") + "/models"
		if key != "" {
			headers["Authorization"] = "Bearer " + key
		}
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := request(ctx, http.MethodGet, endpoint, headers, nil, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// Local models can be slow on big prompts.
var client = &http.Client{Timeout: 5 * time.Minute}

func isRemote(base string) bool {
	u, err := url.Parse(base)
	if err != nil {
		return true
	}
	h := u.Hostname()
	return !(h == "localhost" || h == "127.0.0.1" || h == "::1" || strings.HasSuffix(h, ".local"))
}

func postJSON(ctx context.Context, endpoint string, headers map[string]string, body, out any) error {
	return request(ctx, http.MethodPost, endpoint, headers, body, out)
}

func request(ctx context.Context, method, endpoint string, headers map[string]string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		msg := string(data)
		if len(msg) > 500 {
			msg = msg[:500] + "..."
		}
		he := &HTTPError{Status: resp.StatusCode, Body: msg}
		if s := resp.Header.Get("Retry-After"); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				he.RetryAfter = time.Duration(n) * time.Second
			}
		}
		return he
	}
	return json.Unmarshal(data, out)
}
