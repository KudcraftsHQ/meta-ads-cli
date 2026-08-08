package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Page is one page of a Graph list response.
type Page struct {
	Data   []json.RawMessage `json:"data"`
	Paging *struct {
		Cursors *struct {
			Before string `json:"before"`
			After  string `json:"after"`
		} `json:"cursors,omitempty"`
		Previous string `json:"previous,omitempty"`
		Next     string `json:"next,omitempty"`
	} `json:"paging,omitempty"`
	Summary json.RawMessage `json:"summary,omitempty"`
}

// IsList reports whether a response body is a paginated list rather than a
// single object. Only list responses can be walked with --all.
func IsList(raw json.RawMessage) bool {
	var probe struct {
		Data *json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	if probe.Data == nil {
		return false
	}
	var arr []json.RawMessage
	return json.Unmarshal(*probe.Data, &arr) == nil
}

// PageFunc receives each page as it arrives. Returning an error stops the walk.
type PageFunc func(page *Page) error

// Walk follows paging.next until the API stops offering one, maxItems have
// been seen, or fn returns an error.
//
// The `next` URL Meta hands back is absolute and already carries the cursor and
// credentials, so subsequent pages are fetched verbatim rather than rebuilt.
func (c *Client) Walk(ctx context.Context, req Request, maxItems int, fn PageFunc) error {
	raw, err := c.Do(ctx, req)
	if err != nil {
		return err
	}
	if c.DryRun {
		// Nothing was sent, so there is nothing to walk; hand the description
		// straight through.
		return fn(&Page{Data: []json.RawMessage{raw}})
	}

	seen := 0
	for {
		var page Page
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("parse list response: %w", err)
		}
		if page.Data == nil {
			return fmt.Errorf("--all needs a list response, but %s returned a single object", req.Path)
		}

		if maxItems > 0 && seen+len(page.Data) > maxItems {
			page.Data = page.Data[:maxItems-seen]
		}
		seen += len(page.Data)

		if err := fn(&page); err != nil {
			return err
		}

		next := ""
		if page.Paging != nil {
			next = page.Paging.Next
		}
		if next == "" || (maxItems > 0 && seen >= maxItems) {
			return nil
		}
		c.tracef("following next page (%d items so far)", seen)

		raw, err = c.Send(ctx, func() (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		})
		if err != nil {
			return err
		}
	}
}

// WalkAll collects every page into a single list response, preserving the
// summary from the first page.
func (c *Client) WalkAll(ctx context.Context, req Request, maxItems int) (json.RawMessage, error) {
	all := make([]json.RawMessage, 0, 64)
	var summary json.RawMessage
	pages := 0

	err := c.Walk(ctx, req, maxItems, func(p *Page) error {
		if pages == 0 {
			summary = p.Summary
		}
		pages++
		all = append(all, p.Data...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := map[string]any{"data": all}
	if len(summary) > 0 {
		out["summary"] = summary
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return raw, nil
}
