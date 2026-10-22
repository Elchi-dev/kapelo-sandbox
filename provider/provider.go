// Package provider is where the worker's model runs: an OpenAI compatible
// API (OpenRouter, OpenAI, DeepSeek, Ollama, a gateway of your own) or
// Anthropic's. Providers are kept in a small JSON file, written by
// `kapelo-sandbox worker init`; API keys are best left in the environment and
// named there.
package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	KindOpenAI    = "openai-compatible"
	KindAnthropic = "anthropic"
)

type Provider struct {
	// Kind is openai-compatible (the default) or anthropic.
	Kind    string `json:"kind,omitempty"`
	BaseURL string `json:"base_url"`
	// APIKeyEnv names the environment variable with the key. APIKey holds
	// the key itself, for those who prefer it in the file.
	APIKeyEnv string `json:"api_key_env,omitempty"`
	APIKey    string `json:"api_key,omitempty"`
	// Model is the one a task starts with; Escalate the stronger one the
	// third attempt gets.
	Model       string `json:"model"`
	Escalate    string `json:"escalate,omitempty"`
	ContextSize int    `json:"context_size,omitempty"`
}

type Config struct {
	Default   string              `json:"default"`
	Providers map[string]Provider `json:"providers"`
}

// Presets are the providers `init` offers. Models are left to the user:
// they change faster than this list.
var Presets = map[string]Provider{
	"openrouter": {Kind: KindOpenAI, BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "OPENROUTER_API_KEY"},
	"openai":     {Kind: KindOpenAI, BaseURL: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY"},
	"anthropic":  {Kind: KindAnthropic, BaseURL: "https://api.anthropic.com/v1", APIKeyEnv: "ANTHROPIC_API_KEY"},
	"deepseek":   {Kind: KindOpenAI, BaseURL: "https://api.deepseek.com/v1", APIKeyEnv: "DEEPSEEK_API_KEY"},
	"ollama":     {Kind: KindOpenAI, BaseURL: "http://localhost:11434/v1"},
}

// PresetNames in the order init shows them.
func PresetNames() []string {
	return []string{"openrouter", "openai", "anthropic", "deepseek", "ollama"}
}

// Check reports what makes a provider unusable.
func (p Provider) Check() error {
	var errs []error
	if p.Kind != "" && p.Kind != KindOpenAI && p.Kind != KindAnthropic {
		errs = append(errs, fmt.Errorf("kind %q is not %s or %s", p.Kind, KindOpenAI, KindAnthropic))
	}
	u, err := url.Parse(p.BaseURL)
	switch {
	case err != nil || u.Host == "":
		errs = append(errs, fmt.Errorf("base_url %q is not a URL", p.BaseURL))
	case u.Scheme != "https" && !local(u.Hostname()):
		errs = append(errs, fmt.Errorf("base_url must be https, except on this machine (%s)", p.BaseURL))
	}
	if p.Model == "" {
		errs = append(errs, errors.New("no model is set"))
	}
	return errors.Join(errs...)
}

func local(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "host.docker.internal"
}

// Key returns the API key, from the file or the named environment
// variable. A provider without either (such as Ollama) gets a placeholder,
// since the harness insists on some key.
func (p Provider) Key(getenv func(string) string) (string, error) {
	if p.APIKey != "" {
		return p.APIKey, nil
	}
	if p.APIKeyEnv != "" {
		if k := getenv(p.APIKeyEnv); k != "" {
			return k, nil
		}
		return "", fmt.Errorf("%s is not set", p.APIKeyEnv)
	}
	return "none", nil
}

// Path is where the configuration lives: KAPELO_WORKER_CONFIG, or
// kapelo-worker/config.json in the user's configuration folder.
func Path(getenv func(string) string) string {
	if p := getenv("KAPELO_WORKER_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "kapelo-worker", "config.json")
}

// Load reads the configuration; a missing file is an empty one.
func Load(path string) (Config, error) {
	c := Config{Providers: map[string]Provider{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.Providers == nil {
		c.Providers = map[string]Provider{}
	}
	return c, nil
}

// Save writes the configuration readable by its owner only, since it may
// hold a key.
func (c Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Get returns a provider by name, or the default one when name is empty.
func (c Config) Get(name string) (string, Provider, error) {
	if name == "" {
		name = c.Default
	}
	if name == "" {
		return "", Provider{}, errors.New("no provider is set up; run kapelo-sandbox worker init")
	}
	p, ok := c.Providers[name]
	if !ok {
		names := make([]string, 0, len(c.Providers))
		for n := range c.Providers {
			names = append(names, n)
		}
		slices.Sort(names)
		return "", Provider{}, fmt.Errorf("no provider %q; set up are %v", name, names)
	}
	if p.Kind == "" {
		p.Kind = KindOpenAI
	}
	return name, p, nil
}
