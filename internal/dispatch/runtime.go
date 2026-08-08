package dispatch

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/KudcraftsHQ/meta-ads-cli/internal/config"
	"github.com/KudcraftsHQ/meta-ads-cli/internal/graph"
	"github.com/KudcraftsHQ/meta-ads-cli/internal/output"
	"github.com/KudcraftsHQ/meta-ads-cli/internal/store"
)

// Runtime is the state every command shares: resolved credentials, a Graph
// client, and how results should be printed. It is filled in once the global
// flags have been parsed and before any command body runs.
type Runtime struct {
	Config config.Config
	Client *graph.Client
	S3     *store.Client

	Format output.Format
	// FormatExplicit records whether --output was given, which is how discovery
	// commands know to switch from readable text to structured output.
	FormatExplicit bool
	Pretty         bool
	Columns        []string

	Out io.Writer
	Err io.Writer
}

// Render writes a raw JSON response in the configured format.
func (r *Runtime) Render(raw json.RawMessage) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		// Not JSON -- pass it through rather than losing the response.
		_, writeErr := fmt.Fprintln(r.Out, string(raw))
		return writeErr
	}
	return r.RenderValue(v)
}

// RenderValue writes an already-decoded value in the configured format.
func (r *Runtime) RenderValue(v any) error {
	return output.Write(r.Out, r.Format, v, output.Options{
		Pretty:  r.Pretty,
		Columns: r.Columns,
	})
}

// Warnf writes a note to stderr, which keeps stdout clean for piping.
func (r *Runtime) Warnf(format string, args ...any) {
	if r.Err == nil {
		return
	}
	fmt.Fprintf(r.Err, "meta-ads: "+strings.TrimRight(format, "\n")+"\n", args...)
}
