// Package redisclient builds Redis clients whose calls honor their context's deadline.
//
// go-redis applies its own socket timeouts unless told otherwise, five seconds for a
// read by default. A Redis that accepts connections but never answers then holds every
// call for that long, whatever deadline its context has. Limits and the cache only use
// Redis with short deadlines, so they need clients built here.
package redisclient

import (
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// socketTimeout backs up the context's deadline, for a call that has none.
const socketTimeout = time.Second

// New returns a client for a Redis URL.
func New(url string) (*redis.Client, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		// The parse error can contain the URL, and so its password.
		return nil, errors.New("parsing a Redis URL: the URL is malformed")
	}
	return FromOptions(opts), nil
}

// FromOptions returns a client that honors context deadlines, with short socket
// timeouts and no retries. A retry would only spend the caller's deadline again.
func FromOptions(opts *redis.Options) *redis.Client {
	o := *opts
	o.ContextTimeoutEnabled = true
	o.DialTimeout = socketTimeout
	o.ReadTimeout = socketTimeout
	o.WriteTimeout = socketTimeout
	o.PoolTimeout = socketTimeout
	o.MaxRetries = -1
	return redis.NewClient(&o)
}
