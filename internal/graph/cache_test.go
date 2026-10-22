package graph

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func cachedClient(t *testing.T, srv *httptest.Server, token string, dir string, clock *time.Time) *Client {
	c := testClient(t, srv)
	c.Token = token
	c.Cache = &Cache{Dir: dir, TTL: time.Minute, Now: func() time.Time { return *clock }}
	return c
}

func countingServer(calls *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(calls, 1)
		w.Write([]byte(`{"n":` + itoa(int(n)) + `}`))
	}))
}

func TestCacheServesRepeatedGETWithinTTL(t *testing.T) {
	var calls int32
	srv := countingServer(&calls)
	defer srv.Close()
	dir, now := t.TempDir(), time.Now()
	c := cachedClient(t, srv, "token-abcdefgh", dir, &now)

	req := Request{Method: http.MethodGet, Path: "act_1/insights", Params: map[string]any{"level": "ad", "fields": "spend"}}
	a, _ := c.Do(context.Background(), req)
	b, _ := c.Do(context.Background(), req)
	if calls != 1 || string(a) != string(b) {
		t.Fatalf("second GET should be cached: calls %d, %s vs %s", calls, a, b)
	}

	now = now.Add(61 * time.Second)
	if _, err := c.Do(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("expired entry should refetch, calls %d", calls)
	}
}

func TestCacheNeverStoresTheToken(t *testing.T) {
	var calls int32
	srv := countingServer(&calls)
	defer srv.Close()
	dir, now := t.TempDir(), time.Now()
	c := cachedClient(t, srv, "token-abcdefgh", dir, &now)
	c.AppSecret = "app-secret-value"
	if _, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "act_1/ads"}); err != nil {
		t.Fatal(err)
	}
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		raw, _ := os.ReadFile(p)
		if strings.Contains(string(raw), "token-abcdefgh") || strings.Contains(string(raw), c.appSecretProof()) || strings.Contains(p, "token-abcdefgh") {
			t.Errorf("%s holds a credential", p)
		}
		return nil
	})
}

func TestCacheSeparatesTokens(t *testing.T) {
	var calls int32
	srv := countingServer(&calls)
	defer srv.Close()
	dir, now := t.TempDir(), time.Now()
	req := Request{Method: http.MethodGet, Path: "act_1/ads"}
	cachedClient(t, srv, "token-one-aaaaaa", dir, &now).Do(context.Background(), req)
	cachedClient(t, srv, "token-two-bbbbbb", dir, &now).Do(context.Background(), req)
	if calls != 2 {
		t.Errorf("profiles must not share cached responses, calls %d", calls)
	}
}

func TestWriteInvalidatesAndIsNeverCached(t *testing.T) {
	var calls int32
	srv := countingServer(&calls)
	defer srv.Close()
	dir, now := t.TempDir(), time.Now()
	c := cachedClient(t, srv, "token-abcdefgh", dir, &now)

	get := Request{Method: http.MethodGet, Path: "act_1/ads", Params: map[string]any{"fields": "id,status"}}
	post := Request{Method: http.MethodPost, Path: "120200", Params: map[string]any{"status": "PAUSED"}}
	c.Do(context.Background(), get)
	c.Do(context.Background(), post)
	c.Do(context.Background(), post)
	if calls != 3 {
		t.Fatalf("writes must always be sent, calls %d", calls)
	}
	c.Do(context.Background(), get)
	if calls != 4 {
		t.Errorf("a write must invalidate cached reads, calls %d", calls)
	}
}

func TestCacheCoversPaginatedPages(t *testing.T) {
	var calls int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Query().Get("after") == "" {
			w.Write([]byte(`{"data":[{"id":"1"}],"paging":{"next":"` + srv.URL + `/v26.0/act_1/insights?after=c1&access_token=token-abcdefgh"}}`))
			return
		}
		w.Write([]byte(`{"data":[{"id":"2"}]}`))
	}))
	defer srv.Close()
	dir, now := t.TempDir(), time.Now()
	c := cachedClient(t, srv, "token-abcdefgh", dir, &now)
	req := Request{Method: http.MethodGet, Path: "act_1/insights"}
	for i := 0; i < 2; i++ {
		raw, err := c.WalkAll(context.Background(), req, 0)
		if err != nil || !strings.Contains(string(raw), `"2"`) {
			t.Fatalf("walk %d: %s %v", i, raw, err)
		}
	}
	if calls != 2 {
		t.Errorf("second walk should come from cache, calls %d", calls)
	}
}

func TestErrorsAreNotCached(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"bad","code":100}}`))
	}))
	defer srv.Close()
	dir, now := t.TempDir(), time.Now()
	c := cachedClient(t, srv, "token-abcdefgh", dir, &now)
	c.Do(context.Background(), Request{Method: http.MethodGet, Path: "x"})
	c.Do(context.Background(), Request{Method: http.MethodGet, Path: "x"})
	if calls != 2 {
		t.Errorf("errors must not be cached, calls %d", calls)
	}
}
