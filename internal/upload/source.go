// Package upload resolves file arguments and pushes media to Meta.
package upload

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/KudcraftsHQ/meta-ads-cli/internal/store"
)

// Source is a resolved file argument, ready to be read once.
type Source struct {
	// Name is the filename Meta is told about.
	Name string
	// Size is the byte length, or -1 when the origin did not declare one.
	Size int64

	body io.ReadCloser
}

// Read implements io.Reader.
func (s *Source) Read(p []byte) (int, error) { return s.body.Read(p) }

// Close releases the underlying handle.
func (s *Source) Close() error { return s.body.Close() }

// Describe names the accepted forms, for help text.
func Describe() string {
	return "a local path, @path, file:///path, https://... or s3://bucket/key"
}

// Resolve opens a file argument in any of the supported forms.
//
//	./creative.png            a path, with or without a leading @
//	file:///abs/creative.png  a file URL
//	https://host/creative.png a URL fetched over HTTP
//	s3://bucket/key           an object read through the AWS credential chain
func Resolve(ctx context.Context, arg string, s3c *store.Client, httpc *http.Client) (*Source, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return nil, fmt.Errorf("empty file argument")
	}
	arg = strings.TrimPrefix(arg, "@")

	switch {
	case store.IsRef(arg):
		ref, err := store.ParseRef(arg)
		if err != nil {
			return nil, err
		}
		if s3c == nil {
			s3c = store.New("")
		}
		body, size, err := s3c.Open(ctx, ref)
		if err != nil {
			return nil, err
		}
		return &Source{Name: path.Base(ref.Key), Size: size, body: body}, nil

	case strings.HasPrefix(arg, "http://"), strings.HasPrefix(arg, "https://"):
		if httpc == nil {
			httpc = &http.Client{Timeout: 10 * time.Minute}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, arg, nil)
		if err != nil {
			return nil, err
		}
		resp, err := httpc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", arg, err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return nil, fmt.Errorf("fetch %s: http %d", arg, resp.StatusCode)
		}
		name := path.Base(strings.SplitN(strings.TrimPrefix(arg, "https://"), "?", 2)[0])
		if name == "" || name == "/" {
			name = "upload"
		}
		return &Source{Name: name, Size: resp.ContentLength, body: resp.Body}, nil

	case strings.HasPrefix(arg, "file://"):
		p := strings.TrimPrefix(arg, "file://")
		if p == "" {
			return nil, fmt.Errorf("file url has no path: %q", arg)
		}
		return openLocal(p)

	default:
		return openLocal(arg)
	}
}

func openLocal(p string) (*Source, error) {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	info, err := os.Stat(p)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", p, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", p)
	}
	fh, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", p, err)
	}
	return &Source{Name: filepath.Base(p), Size: info.Size(), body: fh}, nil
}

// Buffer reads a source fully into memory. Image uploads need this because
// Meta takes the bytes base64-encoded in a form field.
func Buffer(s *Source) ([]byte, error) {
	defer s.Close()
	return io.ReadAll(s)
}
