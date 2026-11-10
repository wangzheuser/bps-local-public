package modelmap

import (
	"errors"
	"sync"
	"testing"
)

// countingLoader records how many times the rules were loaded.
type countingLoader struct {
	mu    sync.Mutex
	calls int
	rules []Rule
	err   error
}

func (c *countingLoader) load() ([]Rule, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return c.rules, nil
}

func (c *countingLoader) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func TestCacheLoadsLazilyOnce(t *testing.T) {
	l := &countingLoader{rules: []Rule{{"gpt-6*", "target"}}}
	c := NewCache(l.load)

	if l.callCount() != 0 {
		t.Fatal("cache must not load before the first resolve")
	}
	got, err := c.Resolve("gpt-6-astra")
	if err != nil || got != "target" {
		t.Fatalf("resolve = %q, %v", got, err)
	}
	// Repeated lookups reuse the snapshot.
	for i := 0; i < 5; i++ {
		if _, err := c.Resolve("gpt-6-sol"); err != nil {
			t.Fatal(err)
		}
	}
	if l.callCount() != 1 {
		t.Fatalf("loader calls = %d, want 1", l.callCount())
	}
}

func TestCacheInvalidateReloads(t *testing.T) {
	l := &countingLoader{rules: []Rule{{"gpt-6*", "old"}}}
	c := NewCache(l.load)

	if got, _ := c.Resolve("gpt-6-astra"); got != "old" {
		t.Fatalf("got %q", got)
	}
	// Simulate the console saving a new rule set.
	l.mu.Lock()
	l.rules = []Rule{{"gpt-6*", "new"}}
	l.mu.Unlock()
	c.Invalidate()

	if got, _ := c.Resolve("gpt-6-astra"); got != "new" {
		t.Fatalf("after invalidate got %q, want new", got)
	}
	if l.callCount() != 2 {
		t.Fatalf("loader calls = %d, want 2", l.callCount())
	}
}

func TestCacheUnmatchedReturnsEmpty(t *testing.T) {
	l := &countingLoader{rules: []Rule{{"gpt-6*", "target"}}}
	c := NewCache(l.load)
	got, err := c.Resolve("gpt-5.6-sol")
	if err != nil || got != "" {
		t.Fatalf("resolve = %q, %v; want empty with no error", got, err)
	}
}

func TestCacheLoaderErrorIsReported(t *testing.T) {
	l := &countingLoader{err: errors.New("db down")}
	c := NewCache(l.load)
	if _, err := c.Resolve("gpt-6-astra"); err == nil {
		t.Fatal("loader error must surface")
	}
	// A failed load must not poison the cache: the next call retries.
	l.mu.Lock()
	l.err = nil
	l.rules = []Rule{{"gpt-6*", "recovered"}}
	l.mu.Unlock()
	if got, err := c.Resolve("gpt-6-astra"); err != nil || got != "recovered" {
		t.Fatalf("retry = %q, %v", got, err)
	}
}

func TestCacheNilSafety(t *testing.T) {
	var c *Cache
	if got, err := c.Resolve("gpt-6-astra"); err != nil || got != "" {
		t.Fatalf("nil cache must be a no-op, got %q %v", got, err)
	}
	c.Invalidate() // must not panic

	noLoader := NewCache(nil)
	if got, err := noLoader.Resolve("gpt-6-astra"); err != nil || got != "" {
		t.Fatalf("loaderless cache must be a no-op, got %q %v", got, err)
	}
}

func TestCacheConcurrentResolveAndInvalidate(t *testing.T) {
	l := &countingLoader{rules: []Rule{{"gpt-6*", "target"}}}
	c := NewCache(l.load)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := c.Resolve("gpt-6-astra"); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 20; j++ {
			c.Invalidate()
		}
	}()
	wg.Wait()
}
