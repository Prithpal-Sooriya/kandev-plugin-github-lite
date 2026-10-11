package githublite

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestForgetPathPrefixForgetsScopedEntries pins the manual-sync scoping
// contract: forgetting a repository prefix drops that repository's cached
// entries (the next read re-fetches once) while every other cached entry
// survives untouched.
func TestForgetPathPrefixForgetsScopedEntries(t *testing.T) {
	var fetches sync.Map // path -> *int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		if count, ok := fetches.Load(key); ok {
			atomic.AddInt32(count.(*int32), 1)
		} else {
			var fresh int32 = 1
			fetches.Store(key, &fresh)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	c := &client{
		http:    server.Client(),
		baseURL: server.URL,
		ttl:     300 * time.Second,
		cache:   make(map[string]clientCacheEntry),
	}
	ctx := context.Background()

	count := func(path string) int {
		if v, ok := fetches.Load(path); ok {
			return int(atomic.LoadInt32(v.(*int32)))
		}
		return 0
	}

	// Prime two repositories' cache entries.
	var out map[string]any
	if err := c.getJSON(ctx, "/repos/acme/widgets/pulls?head=acme:feature", &out); err != nil {
		t.Fatalf("widgets read: %v", err)
	}
	if err := c.getJSON(ctx, "/repos/acme/widgets/pulls/7", &out); err != nil {
		t.Fatalf("widgets pull read: %v", err)
	}
	if err := c.getJSON(ctx, "/repos/other/gadgets/pulls?head=other:feature", &out); err != nil {
		t.Fatalf("gadgets read: %v", err)
	}
	if got := count("/repos/acme/widgets/pulls?head=acme:feature"); got != 1 {
		t.Fatalf("widgets lookup fetches: %d", got)
	}

	// A per-task sync forgets only the synced task's repository.
	c.forgetPathPrefix("/repos/acme/widgets/")

	// The forgotten paths re-fetch once...
	if err := c.getJSON(ctx, "/repos/acme/widgets/pulls?head=acme:feature", &out); err != nil {
		t.Fatalf("widgets re-read: %v", err)
	}
	if err := c.getJSON(ctx, "/repos/acme/widgets/pulls/7", &out); err != nil {
		t.Fatalf("widgets pull re-read: %v", err)
	}
	if got := count("/repos/acme/widgets/pulls?head=acme:feature"); got != 2 {
		t.Fatalf("forgotten lookup must re-fetch once: %d fetches", got)
	}
	if got := count("/repos/acme/widgets/pulls/7"); got != 2 {
		t.Fatalf("forgotten pull must re-fetch once: %d fetches", got)
	}

	// ...while other repositories' entries stay cache-served.
	if err := c.getJSON(ctx, "/repos/other/gadgets/pulls?head=other:feature", &out); err != nil {
		t.Fatalf("gadgets re-read: %v", err)
	}
	if got := count("/repos/other/gadgets/pulls?head=other:feature"); got != 1 {
		t.Fatalf("unrelated entry must stay cached: %d fetches", got)
	}
}
