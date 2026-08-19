package auth

import (
	"errors"
	"fmt"
)

// ErrMiss is returned when a key is not present.
var ErrMiss = errors.New("auth: cache miss")

// Cache holds tokens keyed by user id.
type Cache struct {
	entries map[string]string
}

// NewCache constructs an empty Cache.
func NewCache() *Cache {
	return &Cache{entries: map[string]string{}}
}

// Get returns the token for a key, or ErrMiss.
func (c *Cache) Get(key string) (string, error) {
	v, ok := c.entries[key]
	if !ok {
		return "", fmt.Errorf("get %q: %w", key, ErrMiss)
	}
	return v, nil
}
