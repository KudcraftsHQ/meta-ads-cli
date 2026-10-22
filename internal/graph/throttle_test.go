package graph

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Header values in the shapes Meta actually sends: JSON strings, the
// business-use-case one keyed by ad account id.
const (
	bucThrottled = `{"1638799833892967":[{"type":"ads_insights","call_count":100,"total_cputime":38,"total_time":71,"estimated_time_to_regain_access":23,"ads_api_access_tier":"development_access"}]}`
	bucOneMinute = `{"1638799833892967":[{"type":"ads_management","call_count":100,"total_cputime":10,"total_time":12,"estimated_time_to_regain_access":1}]}`
	bucHot       = `{"1638799833892967":[{"type":"ads_insights","call_count":82,"total_cputime":20,"total_time":31,"estimated_time_to_regain_access":0}]}`
	adAccount    = `{"acc_id_util_pct":9.67,"reset_time_duration":120,"ads_api_access_tier":"development_access"}`
	appUsage     = `{"call_count":5,"total_cputime":1,"total_time":2}`
)

type sleeps struct{ got []time.Duration }

func (s *sleeps) fn(_ context.Context, d time.Duration) error {
	s.got = append(s.got, d)
	return nil
}

func throttleClient(t *testing.T, srv *httptest.Server) (*Client, *sleeps) {
	c := testClient(t, srv)
	c.MaxWait = DefaultMaxWait
	s := &sleeps{}
	c.sleep = s.fn
	return c, s
}

func TestParseUsage(t *testing.T) {
	h := http.Header{}
	h.Set("X-Business-Use-Case-Usage", bucThrottled)
	h.Set("X-Ad-Account-Usage", adAccount)
	h.Set("X-App-Usage", appUsage)
	u := ParseUsage(h)

	if u.App == nil || u.App.CallCount != 5 {
		t.Errorf("app = %+v", u.App)
	}
	if u.AdAccount == nil || u.AdAccount.UtilPct != 9.67 || u.AdAccount.ResetSeconds != 120 {
		t.Errorf("ad account = %+v", u.AdAccount)
	}
	if len(u.Business) != 1 || u.Business[0].Account != "act_1638799833892967" || u.Business[0].Type != "ads_insights" {
		t.Fatalf("business = %+v", u.Business)
	}
	if regain, acct := u.Regain(); regain != 23*time.Minute || acct != "act_1638799833892967" {
		t.Errorf("regain = %s %s", regain, acct)
	}
	if u.Tier() != "development_access" {
		t.Errorf("tier = %q", u.Tier())
	}
	if m, _ := u.Max(); m.Pct != 100 || m.Name != "ads_insights call_count" {
		t.Errorf("max = %+v", m)
	}
}

func TestParseUsageIgnoresGarbage(t *testing.T) {
	h := http.Header{}
	h.Set("X-Business-Use-Case-Usage", "not json")
	h.Set("X-App-Usage", "{")
	if u := ParseUsage(h); !u.Empty() {
		t.Errorf("malformed headers should parse to nothing, got %+v", u)
	}
}

func throttledServer(calls *int32, buc string, okAfter int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(calls, 1)
		if okAfter > 0 && n > okAfter {
			w.Write([]byte(`{"ok":true}`))
			return
		}
		w.Header().Set("X-Business-Use-Case-Usage", buc)
		w.Header().Set("X-Ad-Account-Usage", adAccount)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"User request limit reached","type":"OAuthException","code":80000,"error_subcode":2446079,"is_transient":true}}`))
	}))
}

func TestLongThrottleFailsFastWithoutRetrying(t *testing.T) {
	var calls int32
	srv := throttledServer(&calls, bucThrottled, 0)
	defer srv.Close()

	c, s := throttleClient(t, srv)
	_, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "act_1638799833892967/insights"})
	var te *ThrottleError
	if !errors.As(err, &te) {
		t.Fatalf("want *ThrottleError, got %v", err)
	}
	if calls != 1 || len(s.got) != 0 {
		t.Errorf("a 23-minute throttle must not be retried: %d calls, sleeps %v", calls, s.got)
	}
	if ExitCode(err) != 4 {
		t.Errorf("exit code = %d, want 4", ExitCode(err))
	}
	msg := err.Error()
	for _, want := range []string{"Meta throttled ad account act_1638799833892967", "regain in ~23 min", "tier: development_access", "code 80000"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "token-abcdefgh") {
		t.Error("throttle error leaked the token")
	}
}

func TestShortThrottleIsWaitedOutExactlyOnce(t *testing.T) {
	var calls int32
	srv := throttledServer(&calls, bucOneMinute, 1)
	defer srv.Close()

	c, s := throttleClient(t, srv)
	raw, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "act_1/ads"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "ok") || calls != 2 {
		t.Errorf("calls = %d, body %s", calls, raw)
	}
	if len(s.got) != 1 || s.got[0] != time.Minute {
		t.Errorf("should sleep exactly the regain time once, slept %v", s.got)
	}
}

func TestShortThrottleTwiceFailsOnSecond(t *testing.T) {
	var calls int32
	srv := throttledServer(&calls, bucOneMinute, 0)
	defer srv.Close()

	c, s := throttleClient(t, srv)
	_, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "act_1/ads"})
	var te *ThrottleError
	if !errors.As(err, &te) {
		t.Fatalf("want *ThrottleError, got %v", err)
	}
	if calls != 2 || len(s.got) != 1 {
		t.Errorf("wait once, then give up: %d calls, sleeps %v", calls, s.got)
	}
}

func TestRetryAfterCountsAsRegain(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) > 1 {
			w.Write([]byte(`{"ok":true}`))
			return
		}
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"Application request limit reached","code":4}}`))
	}))
	defer srv.Close()

	c, s := throttleClient(t, srv)
	if _, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "me"}); err != nil {
		t.Fatal(err)
	}
	if len(s.got) != 1 || s.got[0] != 7*time.Second {
		t.Errorf("slept %v, want [7s]", s.got)
	}
}

func TestThrottleWithoutRegainFailsFast(t *testing.T) {
	for _, code := range []int{4, 17, 32, 613, 80004, 80014} {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"limit","code":` + itoa(code) + `}}`))
		}))
		c, s := throttleClient(t, srv)
		_, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "act_9/ads"})
		srv.Close()
		var te *ThrottleError
		if !errors.As(err, &te) || calls != 1 || len(s.got) != 0 {
			t.Errorf("code %d: err %v, calls %d, sleeps %v", code, err, calls, s.got)
			continue
		}
		if te.Account != "act_9" || !strings.Contains(err.Error(), "regain time not reported") {
			t.Errorf("code %d: %s", code, err)
		}
	}
}

func TestMaxWaitZeroNeverWaits(t *testing.T) {
	var calls int32
	srv := throttledServer(&calls, bucOneMinute, 1)
	defer srv.Close()

	c, s := throttleClient(t, srv)
	c.MaxWait = 0
	if _, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "act_1/ads"}); err == nil {
		t.Fatal("want a throttle error")
	}
	if calls != 1 || len(s.got) != 0 {
		t.Errorf("calls %d, sleeps %v", calls, s.got)
	}
}

func TestUsageWarningPrintsOnceWithoutVerbose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Business-Use-Case-Usage", bucHot)
		w.Header().Set("X-Ad-Account-Usage", adAccount)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := testClient(t, srv)
	var warn strings.Builder
	c.Warn = &warn
	for i := 0; i < 3; i++ {
		if _, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "act_1/insights"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(warn.String(), "warning:"); n != 1 {
		t.Errorf("want exactly one warning, got %d:\n%s", n, warn.String())
	}
	if !strings.Contains(warn.String(), "82%") || !strings.Contains(warn.String(), "act_1638799833892967") {
		t.Errorf("warning = %q", warn.String())
	}
}

func TestNoWarningBelowThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Ad-Account-Usage", adAccount)
		w.Header().Set("X-App-Usage", appUsage)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := testClient(t, srv)
	var warn strings.Builder
	c.Warn = &warn
	if _, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "me"}); err != nil {
		t.Fatal(err)
	}
	if warn.Len() != 0 {
		t.Errorf("unexpected warning %q", warn.String())
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
