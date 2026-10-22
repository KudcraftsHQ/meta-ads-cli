package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Cache keeps successful GET responses on disk for a short TTL, so an agent
// that reads the same campaigns or insights twice in a minute spends one call
// on the shared rate-limit budget instead of two.
//
// Entries live under Dir/<token hash>/, so two profiles never read each
// other's responses, and the token itself is never written anywhere: the key
// is the request URL with access_token and appsecret_proof removed.
type Cache struct {
	Dir string
	TTL time.Duration
	Now func() time.Time
}

type cacheEntry struct {
	StoredAt time.Time       `json:"stored_at"`
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
}

// DefaultCacheDir is $XDG_CACHE_HOME/meta-ads, falling back to the platform's
// user cache directory.
func DefaultCacheDir() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		var err error
		if base, err = os.UserCacheDir(); err != nil {
			return ""
		}
	}
	return filepath.Join(base, "meta-ads")
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte("meta-ads-cache\x00" + token))
	return hex.EncodeToString(sum[:8])
}

// cacheURL is the request URL without credentials, with the query sorted, so
// the same request always maps to the same key.
func cacheURL(u *url.URL) string {
	q := u.Query()
	q.Del("access_token")
	q.Del("appsecret_proof")
	return u.Scheme + "://" + u.Host + u.Path + "?" + q.Encode()
}

func (c *Cache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Cache) bucket(token string) string {
	return filepath.Join(c.Dir, tokenHash(token))
}

func (c *Cache) path(token string, u *url.URL) string {
	sum := sha256.Sum256([]byte(tokenHash(token) + "\n" + cacheURL(u)))
	return filepath.Join(c.bucket(token), hex.EncodeToString(sum[:16])+".json")
}

func (c *Cache) enabled() bool { return c != nil && c.Dir != "" && c.TTL > 0 }

// Get returns a cached body for a GET, if one is younger than the TTL.
func (c *Cache) Get(token string, u *url.URL) (json.RawMessage, bool) {
	if !c.enabled() {
		return nil, false
	}
	p := c.path(token, u)
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	var e cacheEntry
	if json.Unmarshal(raw, &e) != nil || e.URL != cacheURL(u) {
		return nil, false
	}
	if c.now().Sub(e.StoredAt) >= c.TTL {
		_ = os.Remove(p)
		return nil, false
	}
	return e.Body, true
}

// Put stores a successful GET body. Failures are ignored: the cache is an
// optimisation and must never fail a call.
func (c *Cache) Put(token string, u *url.URL, body json.RawMessage) {
	if !c.enabled() || !json.Valid(body) {
		return
	}
	dir := c.bucket(token)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	raw, err := json.Marshal(cacheEntry{StoredAt: c.now(), URL: cacheURL(u), Body: body})
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".put-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(raw)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		return
	}
	if os.Rename(tmp.Name(), c.path(token, u)) != nil {
		_ = os.Remove(tmp.Name())
	}
}

// Invalidate drops cached GETs after a write.
//
// It drops every cached response under the token rather than only those whose
// URL mentions an id the write touched: pausing ad 123 changes what
// act_1/ads returns, and that URL never mentions 123. With a 60 s TTL the
// cost of the broader sweep is a handful of re-fetches; the cost of the
// narrow one is reading a status you just changed and seeing the old one.
func (c *Cache) Invalidate(token string) {
	if c == nil || c.Dir == "" {
		return
	}
	entries, err := os.ReadDir(c.bucket(token))
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			_ = os.Remove(filepath.Join(c.bucket(token), e.Name()))
		}
	}
}
