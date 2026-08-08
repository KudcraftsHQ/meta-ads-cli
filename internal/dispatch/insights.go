package dispatch

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/KudcraftsHQ/meta-ads-cli/internal/config"
	"github.com/KudcraftsHQ/meta-ads-cli/internal/graph"
	"github.com/KudcraftsHQ/meta-ads-cli/internal/tree"
	"github.com/spf13/cobra"
)

// Reporting is most of what anybody does with this API, and the generated form
// of it -- `meta-ads ad-account get-insights --id act_x --level campaign
// --date-preset last_30d --fields ...` -- makes the most common task the most
// verbose one. So insights gets a hand-written command with the defaults
// already filled in.

// Metrics worth having by default: enough to judge a campaign, few enough to
// fit on a screen.
var defaultMetrics = []string{
	"impressions", "clicks", "spend", "reach", "ctr", "cpc", "cpm",
}

// identityFields name the row so a result is readable without cross-referencing.
var identityFields = map[string][]string{
	"account":  {"account_id", "account_name"},
	"campaign": {"campaign_id", "campaign_name"},
	"adset":    {"adset_id", "adset_name", "campaign_name"},
	"ad":       {"ad_id", "ad_name", "adset_name", "campaign_name"},
}

// InsightsCommand reports on an account, campaign, ad set or ad.
func InsightsCommand(rt *Runtime) *cobra.Command {
	var (
		id               string
		level            string
		preset           string
		since            string
		until            string
		fields           []string
		breakdowns       []string
		actionBreakdowns []string
		timeIncrement    string
		filtering        string
		sortBy           []string
		limit            int
		all              bool
		maxItems         int
	)

	cmd := &cobra.Command{
		Use:   "insights [id]",
		Short: "Report on an account, campaign, ad set or ad",
		Long: "Reads the insights edge with sensible defaults: a useful metric set,\n" +
			"identity columns for the level you asked for, and last_30d when no date\n" +
			"range is given.\n\n" +
			"Everything the raw edge accepts is still reachable through\n" +
			"`meta-ads ad-account get-insights`.",
		Example: "  meta-ads insights --level campaign --preset last_7d --output table\n" +
			"  meta-ads insights 23842 --level ad --since 2026-07-01 --until 2026-07-31 --all\n" +
			"  meta-ads insights --level adset --breakdowns publisher_platform --output csv",
		Args:         cobra.MaximumNArgs(1),
		GroupID:      "core",
		SilenceUsage: true,
	}

	f := cmd.Flags()
	f.StringVar(&id, "id", "", "node to report on (defaults to the configured account)")
	f.StringVar(&level, "level", "", "account, campaign, adset or ad")
	f.StringVar(&preset, "preset", "", "a date preset such as last_7d or this_month (default last_30d)")
	f.StringVar(&since, "since", "", "start date, YYYY-MM-DD; overrides --preset")
	f.StringVar(&until, "until", "", "end date, YYYY-MM-DD; defaults to today when --since is given")
	f.StringSliceVar(&fields, "fields", nil, "metrics to return; defaults to a standard set")
	f.StringSliceVar(&breakdowns, "breakdowns", nil, "split rows by, e.g. publisher_platform,age")
	f.StringSliceVar(&actionBreakdowns, "action-breakdowns", nil, "split action metrics by, e.g. action_type")
	f.StringVar(&timeIncrement, "time-increment", "", "days per row, or 'monthly'/'all_days'")
	f.StringVar(&filtering, "filter", "", "JSON array of Meta filtering clauses")
	f.StringSliceVar(&sortBy, "sort", nil, "sort spec, e.g. spend_descending")
	f.IntVar(&limit, "limit", 0, "rows per page")
	f.BoolVar(&all, "all", false, "follow pagination until exhausted")
	f.IntVar(&maxItems, "max-items", 0, "stop after this many rows when using --all")

	registerInsightsCompletions(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		node := strings.TrimSpace(id)
		if len(args) == 1 {
			node = strings.TrimSpace(args[0])
		}
		if node == "" {
			node = rt.Config.AccountID
		}
		if node == "" {
			return fmt.Errorf("no node to report on: pass an id, or set META_ACCOUNT_ID")
		}
		// A bare numeric id at this point is an ad account -- campaign and ad
		// ids are what you get from a previous call and are used verbatim.
		if isNumeric(node) {
			node = config.NormalizeAccountID(node)
		}

		params := map[string]any{}

		if level != "" {
			if _, ok := identityFields[level]; !ok {
				return fmt.Errorf("--level must be one of account, campaign, adset, ad (got %q)", level)
			}
			params["level"] = level
		}

		switch {
		case since != "":
			end := until
			if end == "" {
				end = time.Now().Format("2006-01-02")
			}
			if err := checkDate(since, "--since"); err != nil {
				return err
			}
			if err := checkDate(end, "--until"); err != nil {
				return err
			}
			raw, _ := json.Marshal(map[string]string{"since": since, "until": end})
			params["time_range"] = json.RawMessage(raw)
		case until != "":
			return fmt.Errorf("--until needs --since")
		case preset != "":
			params["date_preset"] = preset
		default:
			params["date_preset"] = "last_30d"
		}

		selected := fields
		if len(selected) == 0 {
			selected = defaultFields(level)
		}
		params["fields"] = strings.Join(selected, ",")

		if len(breakdowns) > 0 {
			params["breakdowns"] = strings.Join(breakdowns, ",")
		}
		if len(actionBreakdowns) > 0 {
			params["action_breakdowns"] = strings.Join(actionBreakdowns, ",")
		}
		if timeIncrement != "" {
			params["time_increment"] = timeIncrement
		}
		if len(sortBy) > 0 {
			params["sort"] = strings.Join(sortBy, ",")
		}
		if limit > 0 {
			params["limit"] = int64(limit)
		}
		if strings.TrimSpace(filtering) != "" {
			if !json.Valid([]byte(filtering)) {
				return fmt.Errorf("--filter must be a JSON array of filtering clauses")
			}
			params["filtering"] = json.RawMessage(filtering)
		}

		req := graph.Request{
			Method: http.MethodGet,
			Path:   node + "/insights",
			Params: params,
		}
		if all {
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

// defaultFields pairs the standard metrics with the columns that identify the
// row at the requested level.
func defaultFields(level string) []string {
	out := []string{"date_start", "date_stop"}
	if ident, ok := identityFields[level]; ok {
		out = append(out, ident...)
	}
	return append(out, defaultMetrics...)
}

func checkDate(v, flag string) error {
	if _, err := time.Parse("2006-01-02", v); err != nil {
		return fmt.Errorf("%s must be YYYY-MM-DD, got %q", flag, v)
	}
	return nil
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// registerInsightsCompletions pulls the accepted values straight out of the
// generated tree, so completion stays correct across API versions without this
// file listing anything by hand.
func registerInsightsCompletions(cmd *cobra.Command) {
	enums := insightsEnums()
	for flag, param := range map[string]string{
		"preset":            "date_preset",
		"level":             "level",
		"breakdowns":        "breakdowns",
		"action-breakdowns": "action_breakdowns",
	} {
		values := enums[param]
		if len(values) == 0 {
			continue
		}
		_ = cmd.RegisterFlagCompletionFunc(flag,
			func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
				return values, cobra.ShellCompDirectiveNoFileComp
			})
	}
}

func insightsEnums() map[string][]string {
	out := map[string][]string{}
	res, err := tree.LoadResource("ad-account")
	if err != nil {
		return out
	}
	op, ok := res.Op("get-insights")
	if !ok {
		return out
	}
	for _, p := range op.Params {
		if len(p.Enum) > 0 {
			out[p.Name] = p.Enum
		}
	}
	return out
}
