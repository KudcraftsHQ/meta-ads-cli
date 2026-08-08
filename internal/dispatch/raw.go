package dispatch

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/KudcraftsHQ/meta-ads-cli/internal/graph"
	"github.com/spf13/cobra"
)

// RawCommand is the escape hatch. The generated tree is only as current as the
// SDK it was built from, so there has to be a way to call an endpoint the tree
// has never heard of.
func RawCommand(rt *Runtime) *cobra.Command {
	var (
		rawParams string
		all       bool
		maxItems  int
		video     bool
	)

	cmd := &cobra.Command{
		Use:   "raw <method> <path>",
		Short: "Call any Graph API endpoint directly",
		Long: "Issues an arbitrary Graph API request with the configured credentials.\n\n" +
			"The path is relative to the API version, so `act_123/ads` becomes\n" +
			"https://graph.facebook.com/v26.0/act_123/ads.",
		Example: "  meta-ads raw GET act_123/ads --params '{\"fields\":\"id,name\",\"limit\":5}'\n" +
			"  meta-ads raw POST act_123/campaigns --params '{\"name\":\"Launch\",\"objective\":\"OUTCOME_SALES\"}'",
		Args:         cobra.ExactArgs(2),
		GroupID:      "core",
		SilenceUsage: true,
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return []string{"GET", "POST", "DELETE"}, cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
	}

	f := cmd.Flags()
	f.StringVar(&rawParams, "params", "", "JSON object of request parameters")
	f.BoolVar(&all, "all", false, "follow pagination until exhausted (GET only)")
	f.IntVar(&maxItems, "max-items", 0, "stop after this many items when using --all")
	f.BoolVar(&video, "video", false, "route to graph-video.facebook.com")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		method := strings.ToUpper(args[0])
		switch method {
		case http.MethodGet, http.MethodPost, http.MethodDelete:
		default:
			return fmt.Errorf("unsupported method %q (the Graph API takes GET, POST or DELETE)", args[0])
		}

		params := map[string]any{}
		if strings.TrimSpace(rawParams) != "" {
			var doc map[string]json.RawMessage
			if err := json.Unmarshal([]byte(rawParams), &doc); err != nil {
				return fmt.Errorf("--params must be a JSON object: %w", err)
			}
			for k, v := range doc {
				params[k] = unquoteScalar(v)
			}
		}

		req := graph.Request{
			Method: method,
			Path:   strings.TrimPrefix(args[1], "/"),
			Params: params,
			Video:  video,
		}

		if all {
			if method != http.MethodGet {
				return fmt.Errorf("--all only applies to GET")
			}
			return runPaginated(cmd.Context(), rt, req, maxItems)
		}
		out, err := rt.Client.Do(cmd.Context(), req)
		if err != nil {
			return err
		}
		return rt.Render(out)
	}
	return cmd
}
