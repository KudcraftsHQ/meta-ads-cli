package graph

import (
	"errors"
	"fmt"
	"strings"
)

// APIError is Meta's error envelope, which every failed Graph call returns in
// the same shape regardless of endpoint.
type APIError struct {
	Status       int    `json:"-"`
	Message      string `json:"message"`
	Type         string `json:"type"`
	Code         int    `json:"code"`
	Subcode      int    `json:"error_subcode"`
	UserTitle    string `json:"error_user_title"`
	UserMessage  string `json:"error_user_msg"`
	TraceID      string `json:"fbtrace_id"`
	IsTransient  bool   `json:"is_transient"`
	RequestPath  string `json:"-"`
	RequestQuery string `json:"-"`
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString("meta api error")
	if e.Status != 0 {
		fmt.Fprintf(&b, " (http %d", e.Status)
		if e.Code != 0 {
			fmt.Fprintf(&b, ", code %d", e.Code)
		}
		if e.Subcode != 0 {
			fmt.Fprintf(&b, ", subcode %d", e.Subcode)
		}
		b.WriteString(")")
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.UserMessage != "" && e.UserMessage != e.Message {
		b.WriteString(" -- ")
		b.WriteString(e.UserMessage)
	}
	if hint := e.Hint(); hint != "" {
		b.WriteString("\n  ")
		b.WriteString(hint)
	}
	if e.TraceID != "" {
		fmt.Fprintf(&b, "\n  fbtrace_id: %s", e.TraceID)
	}
	return b.String()
}

// Meta's documented error codes worth reacting to by name.
const (
	CodeAPIUnknown       = 1
	CodeAPIService       = 2
	CodeTooManyCalls     = 4
	CodeRateLimit        = 17
	CodePermission       = 10
	CodeAppRateLimit     = 32
	CodeInvalidToken     = 190
	CodeAccountRateLimit = 613
	CodeTemporary        = 2601
)

// Throttled reports whether Meta refused the call for rate limiting: the
// app/account limits (4, 17, 32, 613), the Marketing API business-use-case
// limits (80000-80014), or a bare HTTP 429.
func (e *APIError) Throttled() bool {
	switch e.Code {
	case CodeTooManyCalls, CodeRateLimit, CodeAppRateLimit, CodeAccountRateLimit:
		return true
	}
	if e.Code >= 80000 && e.Code <= 80014 {
		return true
	}
	return e.Status == 429
}

// Retryable reports whether re-issuing the request on the exponential backoff
// could plausibly succeed. Throttles are deliberately not retryable here: the
// client waits one out only when Meta says how long it lasts and that fits
// --max-wait (see Client.MaxWait). Blind retries against a blocked account
// keep it blocked.
func (e *APIError) Retryable() bool {
	if e.Throttled() {
		return false
	}
	if e.IsTransient {
		return true
	}
	switch e.Code {
	case CodeAPIUnknown, CodeAPIService, CodeTemporary:
		return true
	}
	return e.Status >= 500
}

// Hint turns the more common failures into something actionable.
func (e *APIError) Hint() string {
	switch e.Code {
	case CodeInvalidToken:
		if e.Subcode == 463 {
			return "the access token has expired -- generate a new one"
		}
		return "the access token is invalid; check META_ACCESS_TOKEN, and that the app secret matches the token's app"
	case CodePermission:
		return "the token lacks permission for this call -- ads_management or ads_read is usually what is missing"
	case CodeRateLimit, CodeAccountRateLimit:
		return "ad account rate limit; `meta-ads usage` shows how close the account is, --max-wait how long the client may wait"
	case CodeAppRateLimit, CodeTooManyCalls:
		return "app-level rate limit; slow down or spread calls across apps"
	}
	if e.Code >= 80000 && e.Code <= 80014 {
		return "business-use-case rate limit on the ad account; `meta-ads usage` shows the budget and regain time"
	}
	if e.Status == 400 && e.Code == 100 {
		return "parameter rejected -- `meta-ads describe <resource> <op>` lists the accepted parameters and their types"
	}
	return ""
}

// ExitCode maps an error onto a shell exit status so scripts can branch.
//
//	1  generic failure          4  rate limited
//	2  usage / bad parameters   5  server-side / transient
//	3  authentication           6  not found
func ExitCode(err error) int {
	var throttle *ThrottleError
	if errors.As(err, &throttle) {
		return 4
	}
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		return 1
	}
	switch apiErr.Code {
	case CodeInvalidToken:
		return 3
	case CodePermission:
		return 3
	}
	if apiErr.Throttled() {
		return 4
	}
	switch {
	case apiErr.Status == 429:
		return 4
	case apiErr.Status == 404:
		return 6
	case apiErr.Status == 401 || apiErr.Status == 403:
		return 3
	case apiErr.Status >= 500:
		return 5
	case apiErr.Status >= 400:
		return 2
	}
	return 1
}

func asAPIError(err error, target **APIError) bool {
	for err != nil {
		if e, ok := err.(*APIError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
