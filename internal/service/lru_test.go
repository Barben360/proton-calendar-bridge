package service

import "testing"

// The LRU keeps at most its capacity, evicting the least recently used.
func TestLRUEvictsLeastRecentlyUsed(t *testing.T) {
	c := newLRU(2)
	c.put("a", []byte("A"))
	c.put("b", []byte("B"))
	if _, ok := c.get("a"); !ok { // a is now the most recently used
		t.Fatal("a missing")
	}
	c.put("c", []byte("C"))
	if _, ok := c.get("b"); ok {
		t.Error("b kept beyond the capacity: the least recently used must go")
	}
	for _, k := range []string{"a", "c"} {
		if _, ok := c.get(k); !ok {
			t.Errorf("%s evicted", k)
		}
	}
	if c.len() != 2 {
		t.Errorf("len = %d, want 2", c.len())
	}
	c.clear()
	if _, ok := c.get("a"); ok || c.len() != 0 {
		t.Error("clear kept entries")
	}
}
