package cache_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/paulo-sampaio/vapor-api/internal/cache"
)

func TestLRUEvictionAndPromotion(t *testing.T) {
	c := cache.New[string, int](2)
	c.Put("a", 1)
	c.Put("b", 2)

	// Access "a" so "b" becomes least recently used.
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("expected a=1, got %v, %v", v, ok)
	}

	c.Put("c", 3)
	if c.Len() != 2 {
		t.Fatalf("expected len 2, got %d", c.Len())
	}
	if _, ok := c.Get("b"); ok {
		t.Fatal("expected b to be evicted")
	}
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("expected a=1, got %v, %v", v, ok)
	}
	if v, ok := c.Get("c"); !ok || v != 3 {
		t.Fatalf("expected c=3, got %v, %v", v, ok)
	}

	// Update existing key in place
	c.Put("c", 30)
	if v, ok := c.Get("c"); !ok || v != 30 {
		t.Fatalf("expected updated c=30, got %v, %v", v, ok)
	}
	if c.Len() != 2 {
		t.Fatalf("expected len 2 after update, got %d", c.Len())
	}
}

func TestLRUConcurrentAccess(t *testing.T) {
	c := cache.New[string, int](64)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				k := fmt.Sprintf("k-%d-%d", id%3, i%40)
				c.Put(k, i)
				c.Get(k)
			}
		}(g)
	}
	wg.Wait()
	if c.Len() > 64 {
		t.Fatalf("cache exceeded capacity: %d", c.Len())
	}
}
