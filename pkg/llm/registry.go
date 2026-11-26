package llm

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ModelConfig selects and configures one model on one provider. It matches
// code-review-bot's ModelConfig so model settings carry across products.
type ModelConfig struct {
	// Provider is a registered provider name: openai, anthropic, gemini,
	// bedrock, azure, ollama, openrouter, groq, deepseek, mistral, xai,
	// together, litellm, or openai-compatible.
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model" json:"model"`

	// APIKey is used as-is. APIKeyEnv names an env var to read it from.
	// When both are empty the provider's conventional env var is used
	// (OPENAI_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY, ...).
	APIKey    string `yaml:"api_key,omitempty" json:"api_key,omitempty"`
	APIKeyEnv string `yaml:"api_key_env,omitempty" json:"api_key_env,omitempty"`

	// BaseURL overrides the provider endpoint (proxies, self-hosted
	// gateways, Azure resource URLs, Ollama hosts).
	BaseURL string            `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// Bedrock: AWS region and optional shared-config profile. Credentials
	// come from the standard AWS chain (env, SSO, IRSA, instance role).
	Region     string `yaml:"region,omitempty" json:"region,omitempty"`
	AWSProfile string `yaml:"aws_profile,omitempty" json:"aws_profile,omitempty"`

	MaxTokens   int      `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty"`
	Temperature *float64 `yaml:"temperature,omitempty" json:"temperature,omitempty"`
	// Timeout bounds one whole call, including reading a stream.
	Timeout    time.Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	MaxRetries int           `yaml:"max_retries,omitempty" json:"max_retries,omitempty"`
}

// ParseSpec parses "provider:model". Only the first colon separates, so
// model IDs that contain colons (Bedrock versions, Ollama tags) are kept
// intact: "bedrock:anthropic.claude-x-v1:0", "ollama:qwen2.5-coder:14b".
func ParseSpec(spec string) (ModelConfig, error) {
	p, m, ok := strings.Cut(strings.TrimSpace(spec), ":")
	if !ok || p == "" || m == "" {
		return ModelConfig{}, fmt.Errorf("model spec %q: want provider:model (providers: %s)", spec, strings.Join(Providers(), ", "))
	}
	return ModelConfig{Provider: strings.ToLower(p), Model: m}, nil
}

// String is the spec form "provider:model".
func (c ModelConfig) String() string { return c.Provider + ":" + c.Model }

// Factory builds a client for a model config.
type Factory func(cfg ModelConfig, logger *slog.Logger) (Client, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register adds a provider. Use it to plug in backends that are not
// built in; built-ins register themselves in init functions.
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	factories[strings.ToLower(name)] = f
}

// Providers lists registered provider names.
func Providers() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(factories))
	for k := range factories {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Known reports whether a provider is registered.
func Known(provider string) bool {
	mu.RLock()
	defer mu.RUnlock()
	_, ok := factories[strings.ToLower(provider)]
	return ok
}

// New builds a client with retries for one model config.
func New(cfg ModelConfig, logger *slog.Logger) (Client, error) {
	if logger == nil {
		logger = slog.Default()
	}
	mu.RLock()
	f, ok := factories[strings.ToLower(cfg.Provider)]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (available: %s)", cfg.Provider, strings.Join(Providers(), ", "))
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("provider %s: model is required", cfg.Provider)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Minute
	}
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = 16384
	}
	c, err := f(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("provider %s: %w", cfg.Provider, err)
	}
	retries := cfg.MaxRetries
	if retries == 0 {
		retries = 3
	}
	if retries < 0 {
		retries = 0
	}
	return WithRetry(c, retries, logger), nil
}

// NewChain builds a primary client with ordered fallbacks. If the primary
// fails after its retries, the next model is tried, and so on.
func NewChain(cfgs []ModelConfig, logger *slog.Logger) (Client, error) {
	if len(cfgs) == 0 {
		return nil, errors.New("no model configured: set ZENITH_MODEL (e.g. anthropic:<model>) or --model")
	}
	if logger == nil {
		logger = slog.Default()
	}
	clients := make([]Client, 0, len(cfgs))
	for _, c := range cfgs {
		cl, err := New(c, logger)
		if err != nil {
			return nil, err
		}
		clients = append(clients, cl)
	}
	if len(clients) == 1 {
		return clients[0], nil
	}
	return &fallback{clients: clients, logger: logger}, nil
}

// resolveKey returns the API key from config or the env vars, in order.
func resolveKey(cfg ModelConfig, envs ...string) string {
	if cfg.APIKey != "" {
		return cfg.APIKey
	}
	if cfg.APIKeyEnv != "" {
		return os.Getenv(cfg.APIKeyEnv)
	}
	for _, e := range envs {
		if v := os.Getenv(e); v != "" {
			return v
		}
	}
	return ""
}

func isNetErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne)
}
