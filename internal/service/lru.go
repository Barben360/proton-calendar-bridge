package service

import (
	"container/list"
	"sync"
)

// lruCapacity is how many rendered objects the service keeps in memory.
const lruCapacity = 2000

// lru holds rendered objects by object ETag, in memory only (SPEC §7.2): an
// ETag names one rendering, so an entry never goes stale. Safe for
// concurrent use; the zero value is not usable (newLRU).
type lru struct {
	mu    sync.Mutex
	limit int
	order *list.List // front: most recently used; values are *lruEntry
	byKey map[string]*list.Element
}

type lruEntry struct {
	key  string
	data []byte
}

func newLRU(capacity int) *lru {
	return &lru{limit: capacity, order: list.New(), byKey: map[string]*list.Element{}}
}

// get returns the object rendered under etag. The bytes are shared: callers
// must not modify them.
func (c *lru) get(etag string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.byKey[etag]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*lruEntry).data, true
}

// put keeps data under etag, evicting the least recently used entry beyond
// the capacity.
func (c *lru) put(etag string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byKey[etag]; ok {
		el.Value.(*lruEntry).data = data
		c.order.MoveToFront(el)
		return
	}
	c.byKey[etag] = c.order.PushFront(&lruEntry{key: etag, data: data})
	for c.order.Len() > c.limit {
		old := c.order.Back()
		c.order.Remove(old)
		delete(c.byKey, old.Value.(*lruEntry).key)
	}
}

// clear drops every entry (logout, relogin, Close).
func (c *lru) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order.Init()
	clear(c.byKey)
}

func (c *lru) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
