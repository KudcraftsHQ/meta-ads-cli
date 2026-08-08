package dispatch

import (
	"encoding/json"
	"fmt"
	"net/http"
	"text/tabwriter"

	"github.com/KudcraftsHQ/meta-ads-cli/internal/config"
	"github.com/KudcraftsHQ/meta-ads-cli/internal/graph"
	"github.com/spf13/cobra"
)

// ConfigCommand inspects credentials. It never prints a token: the useful
// question is "which account am I about to act on", and that can be answered
// without putting a secret on screen or in a shell history file.
func ConfigCommand(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "config",
		Short:        "Inspect profiles and credentials",
		GroupID:      "discovery",
		SilenceUsage: true,
	}

	show := &cobra.Command{
		Use:          "show",
		Short:        "Show the resolved settings for this invocation",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := rt.Config
			view := map[string]any{
				"profile":      c.Profile,
				"config_file":  config.Path(),
				"account_id":   c.AccountID,
				"api_version":  c.APIVersion,
				"access_token": describeSecret(c.AccessToken),
				"app_secret":   describeSecret(c.AppSecret),
			}
			if rt.isStructured() {
				return rt.RenderValue(view)
			}
			tw := tabwriter.NewWriter(rt.Out, 0, 0, 2, ' ', 0)
			for _, k := range []string{"profile", "config_file", "account_id", "api_version", "access_token", "app_secret"} {
				fmt.Fprintf(tw, "%s\t%v\n", k, view[k])
			}
			return tw.Flush()
		},
	}

	profiles := &cobra.Command{
		Use:          "profiles",
		Short:        "List the profiles defined in the config file",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			file, err := config.Load()
			if err != nil {
				return err
			}
			names := file.Names()
			if rt.isStructured() {
				return rt.RenderValue(names)
			}
			if len(names) == 0 {
				fmt.Fprintf(rt.Out, "no profiles defined in %s\n", file.Path)
				return nil
			}
			for _, n := range names {
				fmt.Fprintln(rt.Out, n)
			}
			return nil
		},
	}

	verify := &cobra.Command{
		Use:   "verify",
		Short: "Check the credentials against the API",
		Long:  "Calls /me and, when an account is configured, reads it back -- the fastest way to tell a bad token from a bad account id.",
		Args:  cobra.NoArgs,

		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := rt.Config.RequireToken(); err != nil {
				return err
			}
			result := map[string]any{"profile": rt.Config.Profile}

			raw, err := rt.Client.Do(cmd.Context(), graph.Request{
				Method: http.MethodGet, Path: "me",
				Params: map[string]any{"fields": "id,name"},
			})
			if err != nil {
				return err
			}
			result["me"] = rawValue(raw)

			if rt.Config.AccountID != "" {
				raw, err := rt.Client.Do(cmd.Context(), graph.Request{
					Method: http.MethodGet,
					Path:   config.NormalizeAccountID(rt.Config.AccountID),
					Params: map[string]any{"fields": "id,name,account_status,currency,timezone_name"},
				})
				if err != nil {
					return fmt.Errorf("token is valid but the account is not readable: %w", err)
				}
				result["account"] = rawValue(raw)
			}
			return rt.RenderValue(result)
		},
	}

	cmd.AddCommand(show, profiles, verify)
	return cmd
}

func describeSecret(v string) string {
	if v == "" {
		return "(not set)"
	}
	return fmt.Sprintf("set (%d chars)", len(v))
}

func rawValue(raw []byte) any {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}
