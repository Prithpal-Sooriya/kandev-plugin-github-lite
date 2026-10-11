package githublite

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestIsEmptyJSONBody(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{"[]", true},
		{"  [ ]  ", true},
		{"null", true},
		{"", true},
		{`[{"number":7}]`, false},
		{`{"login":"a"}`, false},
		{`[1,2]`, false},
	}
	for _, c := range cases {
		if got := isEmptyJSONBody([]byte(c.body)); got != c.want {
			t.Errorf("isEmptyJSONBody(%q) = %v, want %v", c.body, got, c.want)
		}
	}
}

// TestCachedEmptyTTLSemantics pins the auto-attach latency contract: an
// EMPTY answer expires after emptyTTL (so the next sweep re-checks and a
// just-opened PR is picked up), while a non-empty answer keeps the normal
// client TTL (so a linked PR does not multiply quota). Burst reads inside
// either TTL are served from cache.
func TestCachedEmptyTTLSemantics(t *testing.T) {
	var body atomic.Value
	body.Store("[]")
	var fetches int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fetches, 1)
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	defer server.Close()

	c := &client{
		http:    server.Client(),
		baseURL: server.URL,
		ttl:     300 * time.Second,
		cache:   make(map[string]clientCacheEntry),
	}
	ctx := context.Background()
	key := http.MethodGet + " /x"
	age := func(d time.Duration) {
		c.mu.Lock()
		entry := c.cache[key]
		entry.fetchURI = time.Now().Add(-d)
		c.cache[key] = entry
		c.mu.Unlock()
	}
	var out []map[string]any

	// An empty answer is fetched once; a burst read inside emptyTTL is
	// served from cache.
	if err := c.getJSONEmptyTTL(ctx, "/x", &out, 45*time.Second); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if err := c.getJSONEmptyTTL(ctx, "/x", &out, 45*time.Second); err != nil {
		t.Fatalf("burst read: %v", err)
	}
	if got := atomic.LoadInt32(&fetches); got != 1 {
		t.Fatalf("fresh empty entry must dedupe bursts: %d fetches", got)
	}

	// The empty answer expires after emptyTTL even though the client TTL
	// is far longer: the next read re-fetches.
	age(46 * time.Second)
	if err := c.getJSONEmptyTTL(ctx, "/x", &out, 45*time.Second); err != nil {
		t.Fatalf("lapsed read: %v", err)
	}
	if got := atomic.LoadInt32(&fetches); got != 2 {
		t.Fatalf("lapsed empty entry must re-fetch: %d fetches", got)
	}

	// A non-empty answer keeps the client TTL: it is served from cache
	// even when older than emptyTTL.
	body.Store(`[{"n":1}]`)
	age(301 * time.Second) // older than everything: force a re-fetch
	if err := c.getJSONEmptyTTL(ctx, "/x", &out, 45*time.Second); err != nil {
		t.Fatalf("positive fetch: %v", err)
	}
	if got := atomic.LoadInt32(&fetches); got != 3 {
		t.Fatalf("expired entry must re-fetch: %d fetches", got)
	}
	age(100 * time.Second) // older than emptyTTL, younger than client TTL
	if err := c.getJSONEmptyTTL(ctx, "/x", &out, 45*time.Second); err != nil {
		t.Fatalf("positive read: %v", err)
	}
	if got := atomic.LoadInt32(&fetches); got != 3 {
		t.Fatalf("non-empty entry must keep the client TTL: %d fetches", got)
	}
}
