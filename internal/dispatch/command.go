package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/KudcraftsHQ/meta-ads-cli/internal/config"
	"github.com/KudcraftsHQ/meta-ads-cli/internal/graph"
	"github.com/KudcraftsHQ/meta-ads-cli/internal/output"
	"github.com/KudcraftsHQ/meta-ads-cli/internal/tree"
	"github.com/spf13/cobra"
)

// Flags this tool owns on every generated operation. A generated parameter with
// one of these names stays reachable through --params.
var reservedFlags = map[string]bool{
	"id": true, "fields": true, "select": true, "params": true,
	"all": true, "max-items": true, "summary": true,
}

// ResourceCommand builds the command for one resource and all its operations.
//
// Only the requested resource is ever built, so a single invocation unmarshals
// one file out of 309 rather than the whole tree.
func ResourceCommand(rt *Runtime, name string) (*cobra.Command, error) {
	res, err := tree.LoadResource(name)
	if err != nil {
		return nil, err
	}

	cmd := &cobra.Command{
		Use:   res.Name,
		Short: fmt.Sprintf("%s (%d operations)", res.Class, len(res.Ops)),
		Long: fmt.Sprintf("Operations on the %s node of the Marketing API.\n\n"+
			"Run `meta-ads describe %s <operation>` for the parameters an operation accepts.",
			res.Class, res.Name),
		GroupID:      "resources",
		SilenceUsage: true,
	}
	for i := range res.Ops {
		cmd.AddCommand(opCommand(rt, res, &res.Ops[i]))
	}
	return cmd, nil
}

func opCommand(rt *Runtime, res *tree.Resource, op *tree.Op) *cobra.Command {
	var (
		id       string
		fields   []string
		rawParam string
		all      bool
		maxItems int
		summary  bool
	)

	isRead := op.Method == http.MethodGet

	cmd := &cobra.Command{
		Use:          op.Name,
		Short:        fmt.Sprintf("%s %s", op.Method, endpointLabel(op)),
		Long:         opLong(res, op),
		Args:         cobra.NoArgs,
		SilenceUsage: true,
	}

	flags := cmd.Flags()
	flags.StringVar(&id, "id", "", "node id to act on (defaults to the configured account for ad-account operations)")
	flags.StringSliceVar(&fields, "fields", nil, "fields to return, comma separated")
	flags.StringSliceVar(&fields, "select", nil, "alias for --fields")
	flags.StringVar(&rawParam, "params", "", "JSON object merged into the request; explicit flags win")
	if isRead {
		flags.BoolVar(&all, "all", false, "follow pagination until exhausted")
		flags.IntVar(&maxItems, "max-items", 0, "stop after this many items when using --all")
		flags.BoolVar(&summary, "summary", false, "ask Meta for the result summary, including total_count")
	}
	_ = flags.MarkHidden("select")

	bindings := bindParams(flags, op, reservedFlags)
	registerCompletions(cmd, res, bindings)

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		node := strings.TrimSpace(id)
		if node == "" {
			node = defaultNode(rt, res)
		}
		if node == "" {
			return fmt.Errorf("--id is required: %s addresses a specific %s node", op.Name, res.Class)
		}
		// Ad account ids are the one node type people habitually type without
		// their prefix, and the API only accepts the act_ form.
		if res.Name == "ad-account" || strings.HasPrefix(res.Name, "ad-account-") {
			node = config.NormalizeAccountID(node)
		}

		explicit, err := collect(flags, bindings)
		if err != nil {
			return err
		}
		params, err := mergeParams(rawParam, explicit)
		if err != nil {
			return err
		}
		if len(fields) > 0 {
			params["fields"] = strings.Join(fields, ",")
		}
		if summary {
			params["summary"] = "true"
		}

		req := graph.Request{
			Method: op.Method,
			Path:   joinPath(node, op.Endpoint),
			Params: params,
		}

		if all && isRead {
			return runPaginated(cmd.Context(), rt, req, maxItems)
		}
		raw, err := rt.Client.Do(cmd.Context(), req)
		if err != nil {
			return err
		}
		return rt.Render(raw)
	}

	return cmd
}

// runPaginated walks every page. ndjson streams item by item so a long sweep
// starts producing output immediately and never has to fit in memory; the other
// formats need the whole set before they can render.
func runPaginated(ctx context.Context, rt *Runtime, req graph.Request, maxItems int) error {
	if rt.Format == output.NDJSON {
		enc := json.NewEncoder(rt.Out)
		enc.SetEscapeHTML(false)
		return rt.Client.Walk(ctx, req, maxItems, func(p *graph.Page) error {
			for _, item := range p.Data {
				var v any
				if err := json.Unmarshal(item, &v); err != nil {
					return err
				}
				if err := enc.Encode(v); err != nil {
					return err
				}
			}
			return nil
		})
	}
	raw, err := rt.Client.WalkAll(ctx, req, maxItems)
	if err != nil {
		return err
	}
	return rt.Render(raw)
}

// defaultNode fills in --id for the resources where the ad account is the only
// sensible target, so the configured account does not have to be repeated.
func defaultNode(rt *Runtime, res *tree.Resource) string {
	if rt.Config.AccountID == "" {
		return ""
	}
	if res.Name == "ad-account" || strings.HasPrefix(res.Name, "ad-account-") {
		return rt.Config.AccountID
	}
	return ""
}

// joinPath assembles the request path from a node id and the operation's
// endpoint, which is "/" for node operations and "/edge" for edges.
func joinPath(node, endpoint string) string {
	endpoint = strings.Trim(endpoint, "/")
	if endpoint == "" {
		return node
	}
	return node + "/" + endpoint
}

func endpointLabel(op *tree.Op) string {
	if op.APIType == tree.TypeNode || strings.Trim(op.Endpoint, "/") == "" {
		return "{id}"
	}
	return "{id}/" + strings.Trim(op.Endpoint, "/")
}

func opLong(res *tree.Resource, op *tree.Op) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s on %s.\n\n", op.Method, endpointLabel(op), res.Class)
	fmt.Fprintf(&b, "Accepts %d parameters", len(op.Params))
	if op.Method == http.MethodGet && op.APIType == tree.TypeEdge {
		b.WriteString("; returns a list, so --all works here")
	}
	b.WriteString(".\n")
	if len(res.Fields) > 0 {
		fmt.Fprintf(&b, "\n%d fields are selectable with --fields; `meta-ads describe %s --fields` lists them.\n",
			len(res.Fields), res.Name)
	}
	return b.String()
}

// registerCompletions wires shell completion for the values worth completing:
// field names on --fields, and the known values of every enum parameter.
func registerCompletions(cmd *cobra.Command, res *tree.Resource, bindings []*binding) {
	if len(res.Fields) > 0 {
		_ = cmd.RegisterFlagCompletionFunc("fields",
			func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
				return res.Fields, cobra.ShellCompDirectiveNoFileComp
			})
	}
	for _, b := range bindings {
		values := b.param.Enum
		if len(values) == 0 {
			continue
		}
		_ = cmd.RegisterFlagCompletionFunc(b.param.Flag,
			func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
				return values, cobra.ShellCompDirectiveNoFileComp
			})
	}
}
