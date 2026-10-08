package githublite

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kandev/kandev/pkg/pluginsdk"
)

// providerID is the repository provider identity this plugin registers.
// "github" itself is reserved by kandev's core GitHub integration; when that
// integration is not authenticated, inspectURL returns null for github.com
// URLs and ownership falls through here.
const providerID = "github-lite"

// referenceSource matches manifest.yaml's reference_sources entry.
const referenceSource = "github_lite_pull_requests"

// providerHost is the display host baked into every provider Repository.
const providerHost = "github.com"

// Config holds the plugin's operator-editable settings, read once per process
// from Host.GetConfig. Kandev restarts the plugin process when config changes,
// so a once-per-process read stays correct.
type Config struct {
	Token        string
	BaseURL      string
	CacheTTL     int64
	AutoAttach   bool
	MaxListPages int64
}

func configFromHost(host pluginsdk.Host) (Config, error) {
	raw, err := host.GetConfig(context.Background())
	if err != nil {
		return Config{}, fmt.Errorf("github-lite: reading config: %w", err)
	}
	config := Config{AutoAttach: true, CacheTTL: 300, MaxListPages: 3}
	if value, ok := raw["github_token"].(string); ok {
		config.Token = value
	}
	if value, ok := raw["github_api_url"].(string); ok && value != "" {
		config.BaseURL = value
	}
	if value, ok := toInt(raw["cache_ttl_seconds"]); ok && value > 0 {
		config.CacheTTL = value
	}
	if value, ok := raw["auto_attach"].(bool); ok {
		config.AutoAttach = value
	}
	if value, ok := toInt(raw["max_list_pages"]); ok && value > 0 {
		config.MaxListPages = value
	}
	return config, nil
}

func toInt(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case string:
		parsed, err := strconv.ParseInt(typed, 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

// Adapters wires the concrete GitHub implementation into the vendored
// source-control recipe Extension. Every adapter lazily resolves its Host
// (injected after Serve) and reads config once per process.
type Adapters struct {
	host func() pluginsdk.Host

	initOnce sync.Once
	initErr  error
	client   *client
	config   Config

	baseURLOverride string // tests only: point at an httptest server

	scopeMu sync.Mutex
	scope   string // connection scope: token owner's login, cached per process
}

// NewAdapters returns adapters that resolve the Host lazily on first use.
func NewAdapters(host func() pluginsdk.Host) *Adapters {
	return &Adapters{host: host}
}

// SetBaseURLOverride points the GitHub client at a different base URL.
// Tests use it to serve an httptest fake; it must be set before first use.
func (a *Adapters) SetBaseURLOverride(baseURL string) {
	a.baseURLOverride = strings.TrimRight(strings.TrimSpace(baseURL), "/")
}

// init reads config and performs the single eager-ish Host API call —
// GET /user to resolve the connection scope (the token owner's login). It
// runs once per process, on the first adapter call.
func (a *Adapters) init(ctx context.Context) error {
	a.initOnce.Do(func() {
		host := a.host()
		if host == nil {
			a.initErr = fmt.Errorf("github-lite: Host not injected yet")
			return
		}
		config, err := configFromHost(host)
		if err != nil {
			a.initErr = err
			return
		}
		a.config = config
		baseURL := config.BaseURL
		if a.baseURLOverride != "" {
			baseURL = a.baseURLOverride
		}
		a.client = newClient(config.Token, baseURL, secondsDuration(config.CacheTTL))
		if !a.client.authenticated() {
			a.initErr = fmt.Errorf("github-lite: no GitHub access token configured — set one at Settings > Plugins > GitHub Lite")
			return
		}
	})
	return a.initErr
}

func secondsDuration(seconds int64) time.Duration {
	return time.Duration(seconds) * time.Second
}
