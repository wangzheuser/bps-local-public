package modelmap

import (
	"fmt"
	"sync"
)

// Source loads the current rule set. Implemented by *store.DB in production and
// by a stub in tests.
type Source interface {
	ListModelMappings() ([]struct{ SourceModel, TargetModel string }, error)
}

// Loader returns the rules to match against.
type Loader func() ([]Rule, error)

// Cache holds the rule set in memory. Wildcard matching needs every rule, so a
// per-request query would turn into a full table scan on each call; the rules
// are loaded once and reloaded only after the console saves them.
type Cache struct {
	mu     sync.RWMutex
	load   Loader
	rules  []Rule
	loaded bool
}

// NewCache builds a cache backed by load. A nil loader disables mapping.
func NewCache(load Loader) *Cache {
	return &Cache{load: load}
}

// Invalidate drops the cached rules so the next Resolve reloads them. The
// console calls this after saving, which keeps edits effective immediately
// without a per-request reload.
func (c *Cache) Invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.loaded = false
	c.rules = nil
	c.mu.Unlock()
}

// Resolve maps model to its target. An empty target with a nil error means no
// rule matched, so the caller forwards the model unchanged.
func (c *Cache) Resolve(model string) (string, error) {
	if c == nil || c.load == nil || model == "" {
		return "", nil
	}
	rules, err := c.snapshot()
	if err != nil {
		return "", err
	}
	target, ok := Resolve(rules, model)
	if !ok {
		return "", nil
	}
	return target, nil
}

// snapshot returns the cached rules, loading them on first use.
func (c *Cache) snapshot() ([]Rule, error) {
	c.mu.RLock()
	if c.loaded {
		rules := c.rules
		c.mu.RUnlock()
		return rules, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	// Another goroutine may have loaded while we waited for the write lock.
	if c.loaded {
		return c.rules, nil
	}
	rules, err := c.load()
	if err != nil {
		// Leave the cache unloaded so a later request retries; meanwhile the
		// caller treats the failure as "no mapping" rather than failing.
		return nil, fmt.Errorf("load model mappings: %w", err)
	}
	c.rules, c.loaded = rules, true
	return rules, nil
}
