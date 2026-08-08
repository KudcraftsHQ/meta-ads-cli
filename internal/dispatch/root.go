package dispatch

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/KudcraftsHQ/meta-ads-cli/internal/config"
	"github.com/KudcraftsHQ/meta-ads-cli/internal/graph"
	"github.com/KudcraftsHQ/meta-ads-cli/internal/output"
	"github.com/KudcraftsHQ/meta-ads-cli/internal/store"
	"github.com/KudcraftsHQ/meta-ads-cli/internal/tree"
	"github.com/spf13/cobra"
)

// Version is set at build time by the release pipeline.
var Version = "dev"

type globals struct {
	profile     string
	accessToken string
	appSecret   string
	accountID   string
	apiVersion  string
	format      string
	pretty      bool
	columns     []string
	dryRun      bool
	verbose     bool
	maxRetries  int
	timeout     time.Duration
	awsRegion   string
}

// NewRoot assembles the command tree for one invocation.
//
// Resource commands are not all registered up front. There are 309 of them and
// registering them would mean parsing every resource file on every run, so
// instead the arguments are inspected and only the resources actually named get
// built. See mountResources.
func NewRoot(args []string, out, errOut io.Writer) *cobra.Command {
	rt := &Runtime{Out: out, Err: errOut, Format: output.JSON}
	g := &globals{}

	root := &cobra.Command{
		Use:   "meta-ads",
		Short: "A command line interface to the Meta Marketing API",
		Long: "meta-ads exposes the Meta Marketing API as commands, generated from Meta's\n" +
			"own SDK so the surface stays complete rather than curated.\n\n" +
			"Start with `meta-ads list` to see the resources, `meta-ads describe\n" +
			"<resource> <op>` to see what an operation takes, and `meta-ads insights`\n" +
			"for reporting.",
		SilenceUsage:      true,
		SilenceErrors:     true,
		Version:           Version,
		CompletionOptions: cobra.CompletionOptions{HiddenDefaultCmd: false},

		// Resources are not registered until they are named, so cobra's own
		// "unknown command" suggestions cannot see them. Handling unmatched
		// arguments here is what makes a typo'd resource name suggest the
		// right one instead of dead-ending.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			if hints := tree.Suggest(args[0], 5); len(hints) > 0 {
				return fmt.Errorf("unknown command %q; did you mean: %s",
					args[0], strings.Join(hints, ", "))
			}
			return fmt.Errorf("unknown command %q (`meta-ads list` shows every resource)", args[0])
		},
	}

	pf := root.PersistentFlags()
	pf.StringVar(&g.profile, "profile", "", "profile from the config file to use")
	pf.StringVar(&g.accessToken, "access-token", "", "access token (overrides META_ACCESS_TOKEN)")
	pf.StringVar(&g.appSecret, "app-secret", "", "app secret, used to sign requests with appsecret_proof")
	pf.StringVar(&g.accountID, "account-id", "", "default ad account, with or without the act_ prefix")
	pf.StringVar(&g.apiVersion, "api-version", "", "Graph API version to call")
	pf.StringVarP(&g.format, "output", "o", "json", "output format: "+strings.Join(output.Formats(), ", "))
	pf.BoolVar(&g.pretty, "pretty", false, "indent JSON output")
	pf.StringSliceVar(&g.columns, "columns", nil, "column order for table and csv output")
	pf.BoolVar(&g.dryRun, "dry-run", false, "print the request that would be sent, without sending it")
	pf.BoolVarP(&g.verbose, "verbose", "v", false, "log requests and rate-limit headers to stderr")
	pf.IntVar(&g.maxRetries, "max-retries", 3, "how many times to retry a throttled or transient failure")
	pf.DurationVar(&g.timeout, "timeout", 2*time.Minute, "per-request timeout")
	pf.StringVar(&g.awsRegion, "aws-region", "", "AWS region for s3:// sources")

	_ = root.RegisterFlagCompletionFunc("output",
		func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			return output.Formats(), cobra.ShellCompDirectiveNoFileComp
		})

	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		return setup(rt, g, cmd)
	}

	root.AddGroup(
		&cobra.Group{ID: "core", Title: "Common commands:"},
		&cobra.Group{ID: "discovery", Title: "Finding your way around:"},
	)

	root.AddCommand(
		InsightsCommand(rt),
		RawCommand(rt),
		ImageCommand(rt),
		VideoCommand(rt),
		S3Command(rt),
		ListCommand(rt),
		DescribeCommand(rt),
		TreeCommand(rt),
		ConfigCommand(rt),
	)

	mountResources(root, rt, args)
	root.SetHelpTemplate(helpTemplate(root))
	return root
}

// setup resolves credentials and builds the client once the global flags have
// been parsed but before any command body runs.
func setup(rt *Runtime, g *globals, cmd *cobra.Command) error {
	meta, err := tree.LoadMeta()
	if err != nil {
		return err
	}

	cfg, err := config.Resolve(config.Overrides{
		Profile:     g.profile,
		AccessToken: g.accessToken,
		AppSecret:   g.appSecret,
		AccountID:   g.accountID,
		APIVersion:  g.apiVersion,
	}, meta.APIVersion)
	if err != nil {
		return err
	}
	cfg.AccountID = config.NormalizeAccountID(cfg.AccountID)
	rt.Config = cfg

	format, err := output.Parse(g.format)
	if err != nil {
		return err
	}
	rt.Format = format
	rt.FormatExplicit = cmd.Flags().Changed("output")
	rt.Pretty = g.pretty
	rt.Columns = g.columns

	var trace io.Writer
	if g.verbose {
		trace = rt.Err
	}
	// META_GRAPH_URL exists for tests and for anyone routing through a proxy;
	// it is deliberately not a flag, because pointing credentials at an
	// arbitrary host should take more than a typo.
	baseURL, videoURL := meta.GraphURL, meta.GraphVideoURL
	if override := os.Getenv("META_GRAPH_URL"); override != "" {
		baseURL, videoURL = override, override
	}

	rt.Client = graph.New(graph.Options{
		Token:      cfg.AccessToken,
		AppSecret:  cfg.AppSecret,
		APIVersion: cfg.APIVersion,
		BaseURL:    baseURL,
		VideoURL:   videoURL,
		MaxRetries: g.maxRetries,
		Timeout:    g.timeout,
		DryRun:     g.dryRun,
		Trace:      trace,
	})
	rt.S3 = store.New(g.awsRegion)

	// Discovery and config commands work offline, so a missing token should
	// not stop them; anything that talks to the API needs one.
	if needsToken(cmd) && !g.dryRun {
		if err := cfg.RequireToken(); err != nil {
			return err
		}
	}
	return nil
}

// needsToken reports whether a command will actually call the API.
func needsToken(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "list", "describe", "tree", "completion", "help", "config":
			return false
		}
	}
	return true
}

// mountResources builds the resource commands the arguments actually name.
//
// Checking a name is a lookup in the embedded filesystem, so the cost of this
// scan is negligible and the cost of building is paid only for the one or two
// resources involved. A flag value that happens to match a resource name mounts
// a command that is then never executed, which is harmless.
func mountResources(root *cobra.Command, rt *Runtime, args []string) {
	mounted := map[string]bool{}
	for _, arg := range args {
		if arg == "" || strings.HasPrefix(arg, "-") || mounted[arg] {
			continue
		}
		if !tree.HasResource(arg) {
			continue
		}
		cmd, err := ResourceCommand(rt, arg)
		if err != nil {
			continue
		}
		if len(mounted) == 0 {
			// Added on demand: an empty group heading in --help would be a
			// puzzle, since the resources are not listed there.
			root.AddGroup(&cobra.Group{ID: "resources", Title: "Generated API resources:"})
		}
		mounted[arg] = true
		root.AddCommand(cmd)
	}
}

// helpTemplate appends the resource count, which the command list cannot show
// because the resources are not registered until they are named.
func helpTemplate(root *cobra.Command) string {
	names, err := tree.ResourceNames()
	count := len(names)
	suffix := ""
	if err == nil && count > 0 {
		suffix = fmt.Sprintf("\n%d API resources are available as commands but are not listed here.\n"+
			"Run `meta-ads list` to see them, then `meta-ads <resource> --help`.\n", count)
	}
	return root.HelpTemplate() + suffix
}

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	root := NewRoot(os.Args[1:], os.Stdout, os.Stderr)
	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "meta-ads: %s\n", err)
		return graph.ExitCode(err)
	}
	return 0
}
