package dispatch

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/KudcraftsHQ/meta-ads-cli/internal/config"
	"github.com/KudcraftsHQ/meta-ads-cli/internal/graph"
	"github.com/KudcraftsHQ/meta-ads-cli/internal/output"
	"github.com/spf13/cobra"
)

// UsageCommand spends one cheap call to read Meta's rate-limit headers, so an
// agent can see how much budget an ad account has left before a bulk job.
func UsageCommand(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "usage [account-id]",
		Short: "Show the ad account's rate-limit usage and access tier",
		Long: "Makes one cheap call (GET act_<id>?fields=id), never cached and never\n" +
			"retried, and prints what Meta's rate-limit headers say about it:\n\n" +
			"  X-Business-Use-Case-Usage  per-ad-account budget by call type (ads_insights,\n" +
			"                             ads_management, ...): call_count, total_cputime,\n" +
			"                             total_time in percent, and estimated_time_to_regain_access\n" +
			"                             in minutes when throttled\n" +
			"  X-Ad-Account-Usage         acc_id_util_pct, reset_time_duration, ads_api_access_tier\n" +
			"  X-App-Usage                the app-wide budget\n\n" +
			"Run it before a bulk job. Every caller on the ad account shares the same\n" +
			"budget, so at 75% or more on any metric, wait or spread the work out.\n" +
			"Exit status 4 means the account is throttled right now; the usage is still printed.",
		Example: "  meta-ads usage\n  meta-ads usage act_123 -o table",
		Args:    cobra.MaximumNArgs(1),
		GroupID: "core",
		RunE: func(cmd *cobra.Command, args []string) error {
			account := rt.Config.AccountID
			if len(args) == 1 {
				account = config.NormalizeAccountID(args[0])
			}
			if account == "" {
				return fmt.Errorf("no ad account: pass one, or set --account-id / META_ACCOUNT_ID")
			}

			c := rt.Client
			// The point is a fresh reading: no cache, and no sleeping through a
			// throttle -- report it instead.
			c.Cache, c.MaxWait = nil, 0
			raw, callErr := c.Do(cmd.Context(), graph.Request{
				Method: http.MethodGet,
				Path:   account,
				Params: map[string]any{"fields": "id"},
			})
			if c.DryRun {
				return rt.Render(raw)
			}
			var throttle *graph.ThrottleError
			if callErr != nil && !errors.As(callErr, &throttle) {
				return callErr
			}

			if c.LastUsage.Empty() {
				rt.Warnf("Meta sent no rate-limit headers on this call")
			}
			if err := rt.RenderValue(usageReport(account, c.LastUsage, rt.Format)); err != nil {
				return err
			}
			return callErr
		},
	}
	return cmd
}

// usageReport is a document for JSON, and one row per metric source for the
// row-oriented formats.
func usageReport(account string, u graph.Usage, f output.Format) any {
	regain, _ := u.Regain()
	max, _ := u.Max()
	if f == output.JSON {
		return map[string]any{
			"account":           account,
			"tier":              u.Tier(),
			"max_pct":           max.Pct,
			"max_metric":        max.Name,
			"warn":              max.Pct >= graph.UsageWarnPct,
			"throttled":         regain > 0,
			"regain_minutes":    regain.Minutes(),
			"app":               u.App,
			"ad_account":        u.AdAccount,
			"business_use_case": u.Business,
			"headers_present":   !u.Empty(),
		}
	}

	rows := []map[string]any{}
	if u.App != nil {
		rows = append(rows, map[string]any{
			"scope": "app", "account": "", "type": "",
			"call_count": u.App.CallCount, "total_cputime": u.App.TotalCPUTime, "total_time": u.App.TotalTime,
			"util_pct": "", "regain_min": "", "tier": "",
		})
	}
	if u.AdAccount != nil {
		rows = append(rows, map[string]any{
			"scope": "ad_account", "account": account, "type": "",
			"call_count": "", "total_cputime": "", "total_time": "",
			"util_pct": u.AdAccount.UtilPct, "regain_min": u.AdAccount.ResetSeconds / 60, "tier": u.AdAccount.Tier,
		})
	}
	for _, b := range u.Business {
		rows = append(rows, map[string]any{
			"scope": "business_use_case", "account": b.Account, "type": b.Type,
			"call_count": b.CallCount, "total_cputime": b.TotalCPUTime, "total_time": b.TotalTime,
			"util_pct": "", "regain_min": b.RegainMinutes, "tier": b.Tier,
		})
	}
	return rows
}
