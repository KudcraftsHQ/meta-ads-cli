package dispatch

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/KudcraftsHQ/meta-ads-cli/internal/output"
	"github.com/KudcraftsHQ/meta-ads-cli/internal/tree"
	"github.com/spf13/cobra"
)

// The Marketing API has 309 node types and 1445 operations. Nobody, human or
// model, is going to find their way around that from --help alone, so discovery
// is a first-class part of the tool: list to see what exists, describe to see
// what an operation takes, tree to dump the lot as JSON.

// ListCommand enumerates resources, or the operations on one resource.
func ListCommand(rt *Runtime) *cobra.Command {
	var showOps bool

	cmd := &cobra.Command{
		Use:   "list [resource]",
		Short: "List resources, or the operations on one resource",
		Long: "With no argument, lists every resource in the API tree.\n" +
			"With a resource name, lists the operations available on it.",
		Args:         cobra.MaximumNArgs(1),
		GroupID:      "discovery",
		SilenceUsage: true,
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			names, _ := tree.ResourceNames()
			return names, cobra.ShellCompDirectiveNoFileComp
		},
	}
	cmd.Flags().BoolVar(&showOps, "ops", false, "include every operation when listing all resources")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			return listOps(rt, args[0])
		}
		return listResources(rt, showOps)
	}
	return cmd
}

func listResources(rt *Runtime, withOps bool) error {
	index, err := tree.LoadIndex()
	if err != nil {
		return err
	}
	if rt.isStructured() {
		if withOps {
			return rt.RenderValue(index.Resources)
		}
		rows := make([]map[string]any, 0, len(index.Resources))
		for _, r := range index.Resources {
			rows = append(rows, map[string]any{
				"name":       r.Name,
				"class":      r.Class,
				"operations": len(r.Ops),
			})
		}
		return rt.RenderValue(rows)
	}

	tw := tabwriter.NewWriter(rt.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RESOURCE\tCLASS\tOPS")
	for _, r := range index.Resources {
		fmt.Fprintf(tw, "%s\t%s\t%d\n", r.Name, r.Class, len(r.Ops))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(rt.Err, "\n%d resources. `meta-ads list <resource>` shows its operations.\n", len(index.Resources))
	return nil
}

func listOps(rt *Runtime, name string) error {
	res, err := loadResourceOrSuggest(name)
	if err != nil {
		return err
	}
	if rt.isStructured() {
		return rt.RenderValue(res.Ops)
	}
	tw := tabwriter.NewWriter(rt.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "OPERATION\tMETHOD\tPATH\tPARAMS")
	for i := range res.Ops {
		op := &res.Ops[i]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", op.Name, op.Method, endpointLabel(op), len(op.Params))
	}
	return tw.Flush()
}

// DescribeCommand prints the full signature of an operation.
func DescribeCommand(rt *Runtime) *cobra.Command {
	var showFields bool

	cmd := &cobra.Command{
		Use:   "describe <resource> [operation]",
		Short: "Show the parameters an operation accepts",
		Long: "Prints an operation's HTTP method, path, and every parameter with its\n" +
			"type and -- where the API constrains it -- the accepted values.",
		Args:         cobra.RangeArgs(1, 2),
		GroupID:      "discovery",
		SilenceUsage: true,
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			switch len(args) {
			case 0:
				names, _ := tree.ResourceNames()
				return names, cobra.ShellCompDirectiveNoFileComp
			case 1:
				res, err := tree.LoadResource(args[0])
				if err != nil {
					return nil, cobra.ShellCompDirectiveNoFileComp
				}
				out := make([]string, 0, len(res.Ops))
				for i := range res.Ops {
					out = append(out, res.Ops[i].Name)
				}
				return out, cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
	}
	cmd.Flags().BoolVar(&showFields, "fields", false, "list the resource's selectable fields instead")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		res, err := loadResourceOrSuggest(args[0])
		if err != nil {
			return err
		}
		if showFields {
			if rt.isStructured() {
				return rt.RenderValue(res.Fields)
			}
			for _, f := range res.Fields {
				fmt.Fprintln(rt.Out, f)
			}
			return nil
		}
		if len(args) == 1 {
			return listOps(rt, args[0])
		}
		op, ok := res.Op(args[1])
		if !ok {
			return unknownOp(res, args[1])
		}
		if rt.isStructured() {
			return rt.RenderValue(map[string]any{
				"resource": res.Name,
				"class":    res.Class,
				"op":       op,
			})
		}
		return describeText(rt, res, op)
	}
	return cmd
}

func describeText(rt *Runtime, res *tree.Resource, op *tree.Op) error {
	fmt.Fprintf(rt.Out, "%s %s\n", op.Method, endpointLabel(op))
	fmt.Fprintf(rt.Out, "  meta-ads %s %s --id <%s id> [flags]\n\n", res.Name, op.Name, res.Class)

	if len(op.Params) == 0 {
		fmt.Fprintln(rt.Out, "No parameters beyond --fields.")
		return nil
	}

	tw := tabwriter.NewWriter(rt.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FLAG\tTYPE\tVALUES")
	for _, p := range op.Params {
		values := ""
		if len(p.Enum) > 0 {
			values = strings.Join(p.Enum, " ")
		}
		fmt.Fprintf(tw, "--%s\t%s\t%s\n", p.Flag, p.Type, values)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if op.AllowFileUpload {
		fmt.Fprintln(rt.Out, "\nThis operation accepts a file upload.")
	}
	return nil
}

// TreeCommand dumps the whole catalog, which is the form a program or a model
// wants when it needs to reason about the API surface as a whole.
func TreeCommand(rt *Runtime) *cobra.Command {
	var full bool

	cmd := &cobra.Command{
		Use:          "tree [resource...]",
		Short:        "Dump the API tree as JSON",
		Long:         "Prints the command tree. Naming resources restricts the dump to those.",
		GroupID:      "discovery",
		SilenceUsage: true,
		ValidArgsFunction: func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			names, _ := tree.ResourceNames()
			return names, cobra.ShellCompDirectiveNoFileComp
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "include every parameter (large: ~2 MB across all resources)")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		meta, err := tree.LoadMeta()
		if err != nil {
			return err
		}
		if len(args) > 0 || full {
			names := args
			if len(names) == 0 {
				names, err = tree.ResourceNames()
				if err != nil {
					return err
				}
			}
			resources := make([]*tree.Resource, 0, len(names))
			for _, n := range names {
				res, err := loadResourceOrSuggest(n)
				if err != nil {
					return err
				}
				resources = append(resources, res)
			}
			return rt.RenderValue(map[string]any{
				"api_version": meta.APIVersion,
				"resources":   resources,
			})
		}
		index, err := tree.LoadIndex()
		if err != nil {
			return err
		}
		return rt.RenderValue(map[string]any{
			"api_version": meta.APIVersion,
			"resources":   index.Resources,
		})
	}
	return cmd
}

func loadResourceOrSuggest(name string) (*tree.Resource, error) {
	res, err := tree.LoadResource(name)
	if err == nil {
		return res, nil
	}
	if hints := tree.Suggest(name, 5); len(hints) > 0 {
		return nil, fmt.Errorf("unknown resource %q; did you mean: %s", name, strings.Join(hints, ", "))
	}
	return nil, fmt.Errorf("unknown resource %q (`meta-ads list` shows all of them)", name)
}

func unknownOp(res *tree.Resource, name string) error {
	var near []string
	for i := range res.Ops {
		if strings.Contains(res.Ops[i].Name, name) || strings.Contains(name, res.Ops[i].Name) {
			near = append(near, res.Ops[i].Name)
		}
	}
	if len(near) > 5 {
		near = near[:5]
	}
	if len(near) > 0 {
		return fmt.Errorf("%s has no operation %q; did you mean: %s", res.Name, name, strings.Join(near, ", "))
	}
	return fmt.Errorf("%s has no operation %q (`meta-ads list %s` shows all %d)",
		res.Name, name, res.Name, len(res.Ops))
}

// isStructured reports whether discovery output should be machine-readable.
//
// API responses default to JSON because that is what gets piped and parsed.
// Discovery output defaults to aligned text because that is what gets read --
// but asking for a format explicitly overrides that either way.
func (r *Runtime) isStructured() bool {
	return r.FormatExplicit && r.Format != output.Table
}
