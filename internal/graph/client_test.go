package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := New(Options{
		Token:      "token-abcdefgh",
		APIVersion: "v26.0",
		BaseURL:    srv.URL,
		VideoURL:   srv.URL,
		MaxRetries: 3,
	})
	c.RetryBase = time.Millisecond
	c.RetryCap = 2 * time.Millisecond
	return c
}

func TestAppSecretProof(t *testing.T) {
	// HMAC-SHA256 of the access token keyed by the app secret, cross-checked
	// against `printf 'abc' | openssl dgst -sha256 -hmac secret`.
	const want = "9946dad4e00e913fc8be8e5d3f7e110a4a9e832f83fb09c345285d78638d8a0e"

	if got := New(Options{Token: "abc", AppSecret: "secret"}).appSecretProof(); got != want {
		t.Errorf("appSecretProof = %s, want %s", got, want)
	}
	if got := New(Options{Token: "abc"}).appSecretProof(); got != "" {
		t.Errorf("no app secret should mean no proof, got %q", got)
	}
	if got := New(Options{AppSecret: "secret"}).appSecretProof(); got != "" {
		t.Errorf("no token should mean no proof, got %q", got)
	}
}

func TestGetSendsParamsAsQuery(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Write([]byte(`{"id":"1"}`))
	}))
	defer srv.Close()

	c := testClient(t, srv)
	if _, err := c.Do(context.Background(), Request{
		Method: http.MethodGet, Path: "act_1/ads",
		Params: map[string]any{"fields": "id,name", "limit": int64(5)},
	}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v26.0/act_1/ads" {
		t.Errorf("path = %q", gotPath)
	}
	for _, want := range []string{"fields=id%2Cname", "limit=5", "access_token=token-abcdefgh"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
}

func TestPostSendsParamsAsForm(t *testing.T) {
	var body, contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		buf.ReadFrom(r.Body)
		body, contentType = buf.String(), r.Header.Get("Content-Type")
		w.Write([]byte(`{"id":"1"}`))
	}))
	defer srv.Close()

	c := testClient(t, srv)
	if _, err := c.Do(context.Background(), Request{
		Method: http.MethodPost, Path: "act_1/campaigns",
		Params: map[string]any{"name": "Launch", "special_ad_categories": json.RawMessage(`[]`)},
	}); err != nil {
		t.Fatal(err)
	}
	if contentType != "application/x-www-form-urlencoded" {
		t.Errorf("content type = %q", contentType)
	}
	if !strings.Contains(body, "name=Launch") || !strings.Contains(body, "special_ad_categories=%5B%5D") {
		t.Errorf("body = %q", body)
	}
}

func TestRetriesThrottlingThenSucceeds(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"rate limited","code":17}}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := testClient(t, srv)
	raw, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "me"})
	if err != nil {
		t.Fatalf("should have recovered after retries: %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 attempts, got %d", calls)
	}
	if !strings.Contains(string(raw), `"ok":true`) {
		t.Errorf("body = %s", raw)
	}
}

func TestDoesNotRetryPermanentFailure(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"bad token","code":190,"fbtrace_id":"xyz"}}`))
	}))
	defer srv.Close()

	c := testClient(t, srv)
	_, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "me"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Errorf("an invalid token should not be retried, got %d attempts", calls)
	}
	if code := ExitCode(err); code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	if !strings.Contains(err.Error(), "xyz") {
		t.Errorf("error should carry the trace id: %v", err)
	}
}

func TestWalkFollowsPagination(t *testing.T) {
	var srv *httptest.Server
	page := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page++
		if page < 3 {
			w.Write([]byte(`{"data":[{"id":"` + strings.Repeat("x", page) + `"}],"paging":{"next":"` +
				srv.URL + `/next?page=` + string(rune('0'+page)) + `"}}`))
			return
		}
		w.Write([]byte(`{"data":[{"id":"last"}]}`))
	}))
	defer srv.Close()

	c := testClient(t, srv)
	raw, err := c.WalkAll(context.Background(), Request{Method: http.MethodGet, Path: "act_1/ads"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Data []map[string]string `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 3 {
		t.Fatalf("expected 3 items across 3 pages, got %d: %s", len(out.Data), raw)
	}
	if out.Data[2]["id"] != "last" {
		t.Errorf("last item = %v", out.Data[2])
	}
}

func TestWalkRespectsMaxItems(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"}],"paging":{"next":"` + srv.URL + `/next"}}`))
	}))
	defer srv.Close()

	c := testClient(t, srv)
	raw, err := c.WalkAll(context.Background(), Request{Method: http.MethodGet, Path: "act_1/ads"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Data []map[string]string `json:"data"`
	}
	json.Unmarshal(raw, &out)
	if len(out.Data) != 3 {
		t.Fatalf("max-items 3 should stop at 3, got %d", len(out.Data))
	}
}

func TestDryRunSendsNothing(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	c := testClient(t, srv)
	c.DryRun = true
	raw, err := c.Do(context.Background(), Request{
		Method: http.MethodPost, Path: "act_1/campaigns",
		Params: map[string]any{"name": "Launch"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("dry run must not issue a request")
	}
	var doc struct {
		DryRun bool              `json:"dry_run"`
		Method string            `json:"method"`
		Params map[string]string `json:"params"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.DryRun || doc.Method != "POST" || doc.Params["name"] != "Launch" {
		t.Fatalf("unexpected description: %s", raw)
	}
	if doc.Params["access_token"] == "token-abcdefgh" {
		t.Error("dry run leaked the access token verbatim")
	}
}

func TestVerboseDoesNotLeakToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	var trace bytes.Buffer
	c := testClient(t, srv)
	c.Trace = &trace
	if _, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "me"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(trace.String(), "token-abcdefgh") {
		t.Fatalf("verbose output leaked the access token: %s", trace.String())
	}
	if !strings.Contains(trace.String(), "access_token=") {
		t.Errorf("expected a redacted token in the trace, got: %s", trace.String())
	}
}

func TestEncodeValue(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"plain", "plain"},
		{int64(42), "42"},
		{true, "true"},
		{3.5, "3.5"},
		{json.RawMessage(`{"a":1}`), `{"a":1}`},
	}
	for _, c := range cases {
		got, err := EncodeValue(c.in)
		if err != nil {
			t.Fatalf("%v: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("EncodeValue(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
