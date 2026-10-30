package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/BurntSushi/toml"
)

type Mode string

const (
	Analyze Mode = "analyze" // ask before every fix
	Autofix Mode = "autofix" // apply low/medium-risk fixes automatically
)

type Provider struct {
	Type        string   `toml:"type"` // "openai" (any compatible server) or "anthropic"
	BaseURL     string   `toml:"base_url,omitempty"`
	Model       string   `toml:"model"`
	APIKeyEnv   string   `toml:"api_key_env,omitempty"`
	MaxTokens   int      `toml:"max_tokens,omitempty"`
	Temperature *float64 `toml:"temperature,omitempty"`
}

type Config struct {
	Default     string              `toml:"default"`
	Mode        Mode                `toml:"mode"`
	MaxAttempts int                 `toml:"max_attempts"`
	Fallback    []string            `toml:"fallback,omitempty"` // providers to switch to, in order, when one is exhausted
	Providers   map[string]Provider `toml:"providers"`
}

func Defaults() Config {
	return Config{
		Default:     "local",
		Mode:        Analyze,
		MaxAttempts: 3,
		Providers: map[string]Provider{
			"local": {Type: "openai", BaseURL: "http://localhost:11434/v1", Model: "qwen2.5-coder:7b"},
		},
	}
}

// Dir is ~/.config/lazyfinger on macOS/Linux and %AppData%\lazyfinger on Windows.
func Dir() (string, error) {
	if runtime.GOOS == "windows" {
		d, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(d, "lazyfinger"), nil
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "lazyfinger"), nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".config", "lazyfinger"), nil
}

func Path() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.toml"), nil
}

func Exists() bool {
	p, err := Path()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// LoadUser returns the defaults merged with the user's own config file.
func LoadUser() (Config, error) {
	cfg := Defaults()
	p, err := Path()
	if err != nil {
		return cfg, err
	}
	var user Config
	if ok, err := decode(p, &user); err != nil {
		return cfg, err
	} else if ok {
		cfg.mergeUser(user)
	}
	return cfg, nil
}

// Load adds the project's .lazyfinger.toml on top of the user config.
func Load(projectDir string) (Config, error) {
	cfg, err := LoadUser()
	if err != nil {
		return cfg, err
	}
	var project Config
	if ok, err := decode(filepath.Join(projectDir, ".lazyfinger.toml"), &project); err != nil {
		return cfg, err
	} else if ok {
		cfg.mergeProject(project)
	}
	return cfg, nil
}

// Save writes the user config atomically and returns its path.
func Save(c Config) (string, error) {
	p, err := Path()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	var b bytes.Buffer
	b.WriteString("# lazy configuration. Edit here or run `lazy setup`.\n")
	b.WriteString("# API keys are never stored in this file: they live in your system keychain\n")
	b.WriteString("# or in environment variables.\n\n")
	if err := toml.NewEncoder(&b).Encode(c); err != nil {
		return "", err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o600); err != nil {
		return "", err
	}
	return p, os.Rename(tmp, p)
}

func decode(path string, into *Config) (bool, error) {
	if _, err := toml.DecodeFile(path, into); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

func (c *Config) mergeUser(o Config) {
	if o.Default != "" {
		c.Default = o.Default
	}
	if o.Mode != "" {
		c.Mode = o.Mode
	}
	if o.MaxAttempts > 0 {
		c.MaxAttempts = o.MaxAttempts
	}
	if len(o.Fallback) > 0 {
		c.Fallback = o.Fallback
	}
	if len(o.Providers) > 0 {
		c.Providers = o.Providers // your providers replace the built-in default
	}
}

// Project files live in repos you may have just cloned, so they can only pick
// a provider and an attempt count. They can't define providers or fallbacks
// (a malicious base_url could steal your API key) and can't switch on autofix.
func (c *Config) mergeProject(o Config) {
	if o.Default != "" {
		c.Default = o.Default
	}
	if o.MaxAttempts > 0 && o.MaxAttempts <= 5 {
		c.MaxAttempts = o.MaxAttempts
	}
}

const Example = `# lazy configuration. Run "lazy setup" to edit this visually.
# mode: "analyze" asks before every fix; "autofix" applies safe fixes automatically.
mode = "analyze"
default = "claude"
max_attempts = 3

# When a provider runs out of quota or can't be reached, switch to these in
# order, keeping everything tried so far. Without this line, LazyFinger asks first.
fallback = ["local"]

# Any OpenAI-compatible server: Ollama, LM Studio, llama.cpp, vLLM, OpenRouter, OpenAI...
[providers.local]
type = "openai"
base_url = "http://localhost:11434/v1"   # LM Studio: http://localhost:1234/v1
model = "qwen2.5-coder:7b"               # any model you've pulled

[providers.claude]
type = "anthropic"
model = "claude-sonnet-5-5"
api_key_env = "ANTHROPIC_API_KEY"        # or store the key with "lazy setup"
`
