package graph

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// UsageWarnPct is the share of any rate-limit budget at which the client warns
// on stderr, verbose or not. Past it, the next bulk job is what tips the
// account into a throttle that every other caller on it shares.
const UsageWarnPct = 75.0

// AppUsage is the X-App-Usage header: the app-level budget, in percent.
type AppUsage struct {
	CallCount    float64 `json:"call_count"`
	TotalCPUTime float64 `json:"total_cputime"`
	TotalTime    float64 `json:"total_time"`
}

// AdAccountUsage is the X-Ad-Account-Usage header.
type AdAccountUsage struct {
	UtilPct float64 `json:"acc_id_util_pct"`
	// ResetSeconds is reset_time_duration: seconds until the utilisation resets.
	ResetSeconds float64 `json:"reset_time_duration"`
	Tier         string  `json:"ads_api_access_tier"`
}

// BUCUsage is one entry of X-Business-Use-Case-Usage, the per-ad-account
// budget Meta actually throttles the Marketing API on.
type BUCUsage struct {
	Account      string  `json:"account"`
	Type         string  `json:"type"`
	CallCount    float64 `json:"call_count"`
	TotalCPUTime float64 `json:"total_cputime"`
	TotalTime    float64 `json:"total_time"`
	// RegainMinutes is estimated_time_to_regain_access, which Meta gives in minutes.
	RegainMinutes float64 `json:"estimated_time_to_regain_access"`
	Tier          string  `json:"ads_api_access_tier,omitempty"`
}

// Usage is everything Meta said about rate limits on one response.
type Usage struct {
	App       *AppUsage       `json:"app,omitempty"`
	AdAccount *AdAccountUsage `json:"ad_account,omitempty"`
	Business  []BUCUsage      `json:"business_use_case,omitempty"`
}

// Metric is one percentage out of a Usage, named for a human.
type Metric struct {
	Name    string
	Account string
	Pct     float64
}

// ParseUsage reads the three rate-limit headers. Headers that are absent or
// malformed are skipped: a usage report is advice, never a reason to fail.
func ParseUsage(h http.Header) Usage {
	var u Usage
	if v := h.Get("X-App-Usage"); v != "" {
		var app AppUsage
		if json.Unmarshal([]byte(v), &app) == nil {
			u.App = &app
		}
	}
	if v := h.Get("X-Ad-Account-Usage"); v != "" {
		var acc AdAccountUsage
		if json.Unmarshal([]byte(v), &acc) == nil {
			u.AdAccount = &acc
		}
	}
	if v := h.Get("X-Business-Use-Case-Usage"); v != "" {
		var doc map[string][]BUCUsage
		if json.Unmarshal([]byte(v), &doc) == nil {
			keys := make([]string, 0, len(doc))
			for k := range doc {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				for _, e := range doc[k] {
					e.Account = actID(k)
					u.Business = append(u.Business, e)
				}
			}
		}
	}
	return u
}

// Empty reports whether no usage header was present.
func (u Usage) Empty() bool {
	return u.App == nil && u.AdAccount == nil && len(u.Business) == 0
}

// Metrics lists every percentage in the usage.
func (u Usage) Metrics() []Metric {
	var out []Metric
	if u.App != nil {
		out = append(out,
			Metric{"app call_count", "", u.App.CallCount},
			Metric{"app total_cputime", "", u.App.TotalCPUTime},
			Metric{"app total_time", "", u.App.TotalTime})
	}
	if u.AdAccount != nil {
		out = append(out, Metric{"ad account acc_id_util_pct", "", u.AdAccount.UtilPct})
	}
	for _, b := range u.Business {
		out = append(out,
			Metric{b.Type + " call_count", b.Account, b.CallCount},
			Metric{b.Type + " total_cputime", b.Account, b.TotalCPUTime},
			Metric{b.Type + " total_time", b.Account, b.TotalTime})
	}
	return out
}

// Max is the highest percentage in the usage, and which metric it is.
func (u Usage) Max() (Metric, bool) {
	var best Metric
	found := false
	for _, m := range u.Metrics() {
		if !found || m.Pct > best.Pct {
			best, found = m, true
		}
	}
	return best, found
}

// Regain is the longest estimated_time_to_regain_access across the
// business-use-case entries, and the ad account it applies to.
func (u Usage) Regain() (time.Duration, string) {
	var best time.Duration
	account := ""
	for _, b := range u.Business {
		d := time.Duration(b.RegainMinutes * float64(time.Minute))
		if d > best {
			best, account = d, b.Account
		}
	}
	return best, account
}

// Tier is the Marketing API access tier ("development_access",
// "standard_access"), when Meta reported one.
func (u Usage) Tier() string {
	if u.AdAccount != nil && u.AdAccount.Tier != "" {
		return u.AdAccount.Tier
	}
	for _, b := range u.Business {
		if b.Tier != "" {
			return b.Tier
		}
	}
	return ""
}

// Account is the ad account the usage is about, if any header named one.
func (u Usage) Account() string {
	for _, b := range u.Business {
		if b.Account != "" {
			return b.Account
		}
	}
	return ""
}

// Warning renders the one-line stderr warning for usage at or past
// UsageWarnPct, or "" when every metric is below it.
func (u Usage) Warning() string {
	m, ok := u.Max()
	if !ok || m.Pct < UsageWarnPct {
		return ""
	}
	where := m.Name
	if m.Account != "" {
		where += " on " + m.Account
	}
	tier := ""
	if t := u.Tier(); t != "" {
		tier = ", tier " + t
	}
	return fmt.Sprintf("warning: Meta rate-limit usage at %.0f%% (%s%s); slow down or the account gets throttled for everyone on it -- `meta-ads usage` shows the detail",
		m.Pct, where, tier)
}

var actPattern = regexp.MustCompile(`act_\d+`)

func actID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || strings.HasPrefix(id, "act_") {
		return id
	}
	return "act_" + id
}

// ThrottleError is a request Meta refused for rate limiting whose regain time
// is longer than the client was willing to wait (or unknown).
type ThrottleError struct {
	Account string
	Regain  time.Duration
	Tier    string
	Err     *APIError
}

func (e *ThrottleError) Error() string {
	var b strings.Builder
	b.WriteString("Meta throttled ")
	if e.Account != "" {
		b.WriteString("ad account " + e.Account)
	} else {
		b.WriteString("this app")
	}
	if e.Regain > 0 {
		fmt.Fprintf(&b, " — regain in ~%s", humanDuration(e.Regain))
	} else {
		b.WriteString(" — regain time not reported")
	}
	if e.Tier != "" {
		fmt.Fprintf(&b, " (tier: %s)", e.Tier)
	}
	b.WriteString("\n  not retrying: every call made while throttled extends the block for everyone on the account. ")
	b.WriteString("Wait it out, or raise --max-wait to let the CLI sleep through short ones.")
	if e.Err != nil {
		fmt.Fprintf(&b, "\n  %s", strings.ReplaceAll(e.Err.Error(), "\n", "\n  "))
	}
	return b.String()
}

func (e *ThrottleError) Unwrap() error {
	if e.Err == nil {
		return nil
	}
	return e.Err
}

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d s", int(d.Round(time.Second)/time.Second))
	}
	return fmt.Sprintf("%d min", int((d+time.Minute-1)/time.Minute))
}
