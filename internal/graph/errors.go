package graph

import (
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

// Retryable reports whether re-issuing the request could plausibly succeed.
// Throttling is the common case: a paginated sweep across a large account will
// hit a rate limit sooner or later, and failing the whole script for it is
// worse than waiting a few seconds.
func (e *APIError) Retryable() bool {
	if e.IsTransient {
		return true
	}
	switch e.Code {
	case CodeAPIUnknown, CodeAPIService, CodeTooManyCalls, CodeRateLimit,
		CodeAppRateLimit, CodeAccountRateLimit, CodeTemporary:
		return true
	}
	return e.Status == 429 || e.Status >= 500
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
		return "ad account rate limit; --max-retries controls how long the client waits it out"
	case CodeAppRateLimit, CodeTooManyCalls:
		return "app-level rate limit; slow down or spread calls across apps"
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
	var apiErr *APIError
	if !asAPIError(err, &apiErr) {
		return 1
	}
	switch apiErr.Code {
	case CodeInvalidToken:
		return 3
	case CodePermission:
		return 3
	case CodeRateLimit, CodeAppRateLimit, CodeAccountRateLimit, CodeTooManyCalls:
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
