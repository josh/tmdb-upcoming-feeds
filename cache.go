package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// cache holds memoized string values, persisted as JSON when given a path.
//
// The file is an array of [key, value] pairs, least recently used first.
// Oldest entries are evicted on close until at most max remain.
type cache struct {
	path    string
	max     int
	seq     int
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	value string
	seq   int
}

func loadCache(path string, max int) (*cache, error) {
	c := &cache{path: path, max: max, entries: map[string]*cacheEntry{}}
	if path == "" {
		return c, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	} else if err != nil {
		return nil, err
	}
	var pairs [][2]string
	if err := json.Unmarshal(data, &pairs); err != nil {
		return nil, err
	}
	for _, p := range pairs {
		c.set(p[0], p[1])
	}
	return c, nil
}

func (c *cache) set(key, value string) {
	c.seq++
	c.entries[key] = &cacheEntry{value: value, seq: c.seq}
}

// getOrLoad returns the cached value for key, marking it most recently used,
// or stores and returns load().
func (c *cache) getOrLoad(key string, load func() string) string {
	if e, ok := c.entries[key]; ok {
		c.seq++
		e.seq = c.seq
		return e.value
	}
	v := load()
	c.set(key, v)
	return v
}

func (c *cache) close() error {
	if c.path == "" {
		return nil
	}
	keys := make([]string, 0, len(c.entries))
	for k := range c.entries {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int { return c.entries[a].seq - c.entries[b].seq })
	if len(keys) > c.max {
		keys = keys[len(keys)-c.max:]
	}
	pairs := make([][2]string, len(keys))
	for i, k := range keys {
		pairs[i] = [2]string{k, c.entries[k].value}
	}
	data, err := json.Marshal(pairs)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(c.path, data, 0o644)
}
