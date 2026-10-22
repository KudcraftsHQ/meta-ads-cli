// Package graph is the HTTP layer for the Meta Graph API.
//
// It handles the things every call needs and nothing above them: signing with
// appsecret_proof, encoding parameters, decoding Meta's error envelope, waiting
// out rate limits, and walking cursor pagination.
package graph

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to one Graph API version with one set of credentials.
type Client struct {
	HTTP       *http.Client
	Token      string
	AppSecret  string
	APIVersion string
	BaseURL    string
	VideoURL   string

	// MaxRetries bounds how many times a retryable failure is re-issued.
	MaxRetries int
	// RetryBase is the first backoff interval; each retry doubles it.
	RetryBase time.Duration
	// RetryCap bounds a single backoff interval.
	RetryCap time.Duration

	// DryRun makes every call return a description of the request it would
	// have sent, without sending it.
	DryRun bool
	// Trace, when set, receives a line per request and retry.
	Trace io.Writer

	// MaxWait is the longest throttle the client will sleep through. A
	// throttle whose regain time (estimated_time_to_regain_access or
	// Retry-After) fits is waited out exactly once; anything longer, or with
	// no regain time at all, fails fast with a *ThrottleError. Throttles are
	// never retried on the exponential backoff: every call made while an ad
	// account is blocked extends the block for every caller sharing it.
	MaxWait time.Duration
	// Warn receives one line, once per client, when any rate-limit metric
	// reaches UsageWarnPct. It is independent of Trace.
	Warn io.Writer
	// Cache, when set, serves repeated GETs from disk within its TTL.
	Cache *Cache
	// LastUsage is what Meta's rate-limit headers said on the latest response.
	LastUsage Usage

	warned bool
	sleep  func(ctx context.Context, d time.Duration) error
	rand   *rand.Rand
}

// Options configure a new client.
type Options struct {
	Token      string
	AppSecret  string
	APIVersion string
	BaseURL    string
	VideoURL   string
	MaxRetries int
	Timeout    time.Duration
	DryRun     bool
	Trace      io.Writer
	MaxWait    time.Duration
	Warn       io.Writer
	Cache      *Cache
}

// DefaultMaxWait is how long a throttle may be before the client stops
// waiting for it and fails.
const DefaultMaxWait = 60 * time.Second

// New builds a client, filling in sensible defaults.
func New(o Options) *Client {
	if o.BaseURL == "" {
		o.BaseURL = "https://graph.facebook.com"
	}
	if o.VideoURL == "" {
		o.VideoURL = "https://graph-video.facebook.com"
	}
	if o.Timeout <= 0 {
		o.Timeout = 120 * time.Second
	}
	if o.MaxRetries < 0 {
		o.MaxRetries = 0
	}
	if o.MaxWait < 0 {
		o.MaxWait = 0
	}
	return &Client{
		HTTP:       &http.Client{Timeout: o.Timeout},
		Token:      o.Token,
		AppSecret:  o.AppSecret,
		APIVersion: o.APIVersion,
		BaseURL:    strings.TrimRight(o.BaseURL, "/"),
		VideoURL:   strings.TrimRight(o.VideoURL, "/"),
		MaxRetries: o.MaxRetries,
		RetryBase:  time.Second,
		RetryCap:   32 * time.Second,
		DryRun:     o.DryRun,
		Trace:      o.Trace,
		MaxWait:    o.MaxWait,
		Warn:       o.Warn,
		Cache:      o.Cache,
		sleep:      sleepCtx,
		rand:       rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Request is one Graph API call.
type Request struct {
	Method string
	// Path is relative to the version segment, e.g. "act_123/campaigns".
	Path string
	// Params are already coerced to their wire types by the caller. Values may
	// be string, bool, integer, float, or json.RawMessage for structured ones.
	Params map[string]any
	// Video routes to graph-video.facebook.com, which handles video uploads.
	Video bool
}

// URL builds the absolute URL for a path, without query parameters.
func (c *Client) URL(path string, video bool) string {
	host := c.BaseURL
	if video {
		host = c.VideoURL
	}
	path = strings.TrimLeft(path, "/")
	if c.APIVersion != "" {
		return host + "/" + c.APIVersion + "/" + path
	}
	return host + "/" + path
}

// appSecretProof is the HMAC-SHA256 of the access token keyed by the app
// secret. Meta requires it when the app has "require app secret" enabled, and
// accepts it always, so we send it whenever a secret is configured.
func (c *Client) appSecretProof() string {
	if c.AppSecret == "" || c.Token == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(c.AppSecret))
	mac.Write([]byte(c.Token))
	return hex.EncodeToString(mac.Sum(nil))
}

// Values encodes parameters into form/query values and adds credentials.
func (c *Client) Values(params map[string]any) (url.Values, error) {
	values := url.Values{}
	for k, v := range params {
		encoded, err := EncodeValue(v)
		if err != nil {
			return nil, fmt.Errorf("parameter %q: %w", k, err)
		}
		values.Set(k, encoded)
	}
	if c.Token != "" {
		values.Set("access_token", c.Token)
	}
	if proof := c.appSecretProof(); proof != "" {
		values.Set("appsecret_proof", proof)
	}
	return values, nil
}

// mergePathQuery moves a query string written into the path ("act_1?fields=a,b")
// into values, so the request carries one well-formed query. Appending ours
// after a second "?" would make Meta read "?access_token=…" as part of the last
// parameter's value -- and echo the live token back in its error message.
// Explicit parameters win over ones in the path, and the path can never set
// credentials.
func (c *Client) mergePathQuery(path string, values url.Values) (string, error) {
	base, query, found := strings.Cut(path, "?")
	if !found {
		return path, nil
	}
	parsed, err := url.ParseQuery(query)
	if err != nil {
		return "", fmt.Errorf("query in path %q: %w", base, err)
	}
	for k, vs := range parsed {
		if k == "access_token" || k == "appsecret_proof" {
			return "", fmt.Errorf("don't put %s in the path; credentials come from the profile or META_ACCESS_TOKEN", k)
		}
		if _, set := values[k]; !set && len(vs) > 0 {
			values.Set(k, vs[len(vs)-1])
		}
	}
	return base, nil
}

// scrub masks the credentials this client sends wherever they appear in s.
// Meta sometimes quotes request parameters back in error messages, and Go's
// transport errors carry the full request URL, token included.
func (c *Client) scrub(s string) string {
	for _, secret := range []string{c.Token, c.appSecretProof(), url.QueryEscape(c.Token)} {
		if len(secret) >= 8 {
			s = strings.ReplaceAll(s, secret, redact(secret))
		}
	}
	return s
}

func (c *Client) scrubError(e *APIError) *APIError {
	e.Message = c.scrub(e.Message)
	e.UserMessage = c.scrub(e.UserMessage)
	e.UserTitle = c.scrub(e.UserTitle)
	return e
}

// EncodeValue renders one parameter value the way the Graph API expects it.
func EncodeValue(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "", nil
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case json.RawMessage:
		return string(t), nil
	case []byte:
		return string(t), nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// Do issues a request and returns the raw JSON body.
//
// When DryRun is set nothing is sent; the returned document describes the
// request that would have gone out, with credentials redacted.
func (c *Client) Do(ctx context.Context, req Request) (json.RawMessage, error) {
	values, err := c.Values(req.Params)
	if err != nil {
		return nil, err
	}
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = http.MethodGet
	}
	path, err := c.mergePathQuery(req.Path, values)
	if err != nil {
		return nil, err
	}
	endpoint := c.URL(path, req.Video)

	if c.DryRun {
		return describeRequest(method, endpoint, values), nil
	}

	build := func() (*http.Request, error) {
		if method == http.MethodGet || method == http.MethodDelete {
			r, err := http.NewRequestWithContext(ctx, method, endpoint+"?"+values.Encode(), nil)
			if err != nil {
				return nil, err
			}
			return r, nil
		}
		body := values.Encode()
		r, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return r, nil
	}

	return c.send(ctx, build)
}

// describeRequest renders a dry-run description as a JSON document.
func describeRequest(method, endpoint string, values url.Values) json.RawMessage {
	safe := map[string]string{}
	for k := range values {
		v := values.Get(k)
		if k == "access_token" || k == "appsecret_proof" {
			v = redact(v)
		}
		safe[k] = v
	}
	doc := map[string]any{
		"dry_run": true,
		"method":  method,
		"url":     endpoint,
		"params":  safe,
	}
	raw, _ := json.Marshal(doc)
	return raw
}

// safeURL renders a URL for logging with credentials masked.
//
// url.URL.Redacted only hides userinfo, and the access token travels in the
// query string -- so verbose output would otherwise put a live token into the
// terminal scrollback and any log that captures it.
func safeURL(u *url.URL) string {
	q := u.Query()
	changed := false
	for _, key := range []string{"access_token", "appsecret_proof"} {
		if v := q.Get(key); v != "" {
			q.Set(key, redact(v))
			changed = true
		}
	}
	if !changed {
		return u.Redacted()
	}
	clone := *u
	clone.RawQuery = q.Encode()
	return clone.Redacted()
}

func redact(s string) string {
	if len(s) <= 8 {
		return "<redacted>"
	}
	return s[:4] + "..." + s[len(s)-4:]
}

// Send issues a prepared request with the same retry and error handling as Do.
// Uploads use this to send multipart bodies.
//
// The builder is called once per attempt because a request body cannot be
// replayed.
func (c *Client) Send(ctx context.Context, build func() (*http.Request, error)) (json.RawMessage, error) {
	if c.DryRun {
		r, err := build()
		if err != nil {
			return nil, err
		}
		doc := map[string]any{
			"dry_run": true,
			"method":  r.Method,
			"url":     r.URL.String(),
			"body":    "<multipart or streamed body omitted>",
		}
		raw, _ := json.Marshal(doc)
		return raw, nil
	}
	return c.send(ctx, build)
}

func (c *Client) send(ctx context.Context, build func() (*http.Request, error)) (json.RawMessage, error) {
	waitedThrottle := false
	for attempt := 0; ; attempt++ {
		req, err := build()
		if err != nil {
			return nil, err
		}
		isGet := req.Method == http.MethodGet
		if attempt == 0 && isGet {
			if body, ok := c.Cache.Get(c.Token, req.URL); ok {
				c.tracef("%s %s (cached)", req.Method, safeURL(req.URL))
				return body, nil
			}
		}
		c.tracef("%s %s", req.Method, safeURL(req.URL))

		resp, err := c.HTTP.Do(req)
		if err != nil {
			if attempt >= c.MaxRetries || ctx.Err() != nil || !isTransportRetryable(err) {
				var urlErr *url.Error
				if errors.As(err, &urlErr) {
					// *url.Error prints the whole request URL, access token included.
					return nil, fmt.Errorf("request failed: %s %s: %s", urlErr.Op, safeURL(req.URL), c.scrub(urlErr.Err.Error()))
				}
				return nil, fmt.Errorf("request failed: %s", c.scrub(err.Error()))
			}
			if waitErr := c.backoff(ctx, attempt, 0); waitErr != nil {
				return nil, waitErr
			}
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read response: %w", readErr)
		}
		usage := c.observeUsage(resp.Header)
		if !isGet {
			// A write, successful or not, may have changed what a cached
			// read would return.
			c.Cache.Invalidate(c.Token)
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if len(body) == 0 {
				body = []byte(`{}`)
			}
			if isGet {
				c.Cache.Put(c.Token, req.URL, body)
			}
			return json.RawMessage(body), nil
		}

		apiErr := c.scrubError(parseError(resp.StatusCode, body))
		apiErr.RequestPath = req.URL.Path

		if regain, account, throttled := throttleOf(apiErr, usage, resp.Header); throttled {
			if account == "" {
				account = actPattern.FindString(req.URL.Path)
			}
			if !waitedThrottle && regain > 0 && regain <= c.MaxWait && c.MaxRetries > 0 {
				waitedThrottle = true
				c.warnf("Meta throttled %s; waiting %s as it asked, then trying once more", orApp(account), regain.Round(time.Second))
				if err := c.sleep(ctx, regain); err != nil {
					return nil, err
				}
				continue
			}
			return nil, &ThrottleError{Account: account, Regain: regain, Tier: usage.Tier(), Err: apiErr}
		}

		if attempt >= c.MaxRetries || !apiErr.Retryable() {
			return nil, apiErr
		}
		c.tracef("retryable failure (code %d): %s", apiErr.Code, apiErr.Message)
		if waitErr := c.backoff(ctx, attempt, retryAfter(resp.Header)); waitErr != nil {
			return nil, waitErr
		}
	}
}

// throttleOf decides whether a failed response is a rate-limit refusal, and
// how long Meta said to stay away. The regain time is the longest of
// estimated_time_to_regain_access and Retry-After.
func throttleOf(e *APIError, u Usage, h http.Header) (time.Duration, string, bool) {
	regain, account := u.Regain()
	if ra := retryAfter(h); ra > regain {
		regain = ra
	}
	return regain, account, e.Throttled() || regain > 0
}

func orApp(account string) string {
	if account == "" {
		return "this app"
	}
	return "ad account " + account
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isTransportRetryable(err error) bool {
	// Timeouts and connection resets are worth another go; a malformed URL is
	// not, and neither is a cancelled context.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Timeout() || urlErr.Temporary() || strings.Contains(urlErr.Error(), "connection reset")
	}
	return false
}

// backoff waits before the next attempt: exponential from RetryBase, capped,
// with jitter so concurrent callers do not resynchronise. A Retry-After header
// overrides the computed interval.
func (c *Client) backoff(ctx context.Context, attempt int, server time.Duration) error {
	wait := server
	if wait <= 0 {
		wait = c.RetryBase << attempt
		if wait > c.RetryCap || wait <= 0 {
			wait = c.RetryCap
		}
		jitter := time.Duration(c.rand.Int63n(int64(wait/2) + 1))
		wait = wait/2 + jitter
	}
	c.tracef("waiting %s before retry %d", wait.Round(time.Millisecond), attempt+1)
	return c.sleep(ctx, wait)
}

func retryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

func parseError(status int, body []byte) *APIError {
	var envelope struct {
		Error *APIError `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error != nil {
		envelope.Error.Status = status
		return envelope.Error
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = http.StatusText(status)
	}
	if len(msg) > 500 {
		msg = msg[:500] + "..."
	}
	return &APIError{Status: status, Message: msg}
}

func (c *Client) tracef(format string, args ...any) {
	if c.Trace == nil {
		return
	}
	fmt.Fprintf(c.Trace, "meta-ads: "+format+"\n", args...)
}

// observeUsage records Meta's rate-limit headers, logs them under --verbose,
// and warns once -- verbose or not -- when any metric is at UsageWarnPct.
// They are the only warning you get before a throttle.
func (c *Client) observeUsage(h http.Header) Usage {
	u := ParseUsage(h)
	if !u.Empty() {
		c.LastUsage = u
	}
	if c.Trace != nil {
		for _, key := range []string{"X-App-Usage", "X-Ad-Account-Usage", "X-Business-Use-Case-Usage"} {
			if v := h.Get(key); v != "" {
				c.tracef("%s: %s", key, v)
			}
		}
	}
	if !c.warned {
		if w := u.Warning(); w != "" {
			c.warned = true
			c.warnf("%s", w)
		}
	}
	return u
}

func (c *Client) warnf(format string, args ...any) {
	if c.Warn == nil {
		return
	}
	fmt.Fprintf(c.Warn, "meta-ads: "+format+"\n", args...)
}
