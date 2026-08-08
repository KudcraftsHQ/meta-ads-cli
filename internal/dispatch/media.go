package dispatch

import (
	"fmt"
	"time"

	"github.com/KudcraftsHQ/meta-ads-cli/internal/config"
	"github.com/KudcraftsHQ/meta-ads-cli/internal/store"
	"github.com/KudcraftsHQ/meta-ads-cli/internal/upload"
	"github.com/spf13/cobra"
)

// ImageCommand uploads to an ad account's image library.
func ImageCommand(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "image",
		Short:        "Manage ad images",
		GroupID:      "core",
		SilenceUsage: true,
	}

	var accountID string
	up := &cobra.Command{
		Use:          "upload <file>",
		Short:        "Upload an image and return its hash",
		Long:         "Uploads an image to the ad account's library. The file may be " + upload.Describe() + ".",
		Example:      "  meta-ads image upload ./creative.png\n  meta-ads image upload s3://assets/creative.png --account-id act_123",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			account, err := rt.account(accountID)
			if err != nil {
				return err
			}
			src, err := upload.Resolve(cmd.Context(), args[0], rt.S3, nil)
			if err != nil {
				return err
			}
			raw, err := upload.Image(cmd.Context(), rt.Client, account, src)
			if err != nil {
				return err
			}
			return rt.Render(raw)
		},
	}
	up.Flags().StringVar(&accountID, "account-id", "", "ad account to upload into (defaults to the configured account)")

	cmd.AddCommand(up)
	return cmd
}

// VideoCommand uploads and inspects ad videos.
func VideoCommand(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "video",
		Short:        "Manage ad videos",
		GroupID:      "core",
		SilenceUsage: true,
	}

	var (
		accountID   string
		title       string
		chunkMB     int
		wait        bool
		waitTimeout time.Duration
		quiet       bool
	)

	up := &cobra.Command{
		Use:   "upload <file>",
		Short: "Upload a video in chunks",
		Long: "Uploads a video to the ad account using Meta's chunked upload protocol.\n" +
			"The file may be " + upload.Describe() + ".\n\n" +
			"Meta encodes the video after upload and rejects ad creation against one\n" +
			"that is still processing, so --wait is usually what you want in a script.",
		Example: "  meta-ads video upload ./ad.mp4 --wait\n" +
			"  meta-ads video upload s3://assets/ad.mp4 --title 'Launch cut' --wait",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			account, err := rt.account(accountID)
			if err != nil {
				return err
			}
			src, err := upload.Resolve(cmd.Context(), args[0], rt.S3, nil)
			if err != nil {
				return err
			}
			opts := upload.VideoOptions{
				Name:        title,
				ChunkSize:   int64(chunkMB) << 20,
				Wait:        wait,
				WaitTimeout: waitTimeout,
			}
			if !quiet {
				// Progress goes to stderr so it never contaminates piped output.
				opts.Progress = func(sent, total int64) {
					fmt.Fprintf(rt.Err, "\rmeta-ads: uploaded %d/%d MiB", sent>>20, total>>20)
					if sent >= total {
						fmt.Fprintln(rt.Err)
					}
				}
			}
			raw, err := upload.Video(cmd.Context(), rt.Client, account, src, opts)
			if err != nil {
				return err
			}
			return rt.Render(raw)
		},
	}
	f := up.Flags()
	f.StringVar(&accountID, "account-id", "", "ad account to upload into (defaults to the configured account)")
	f.StringVar(&title, "title", "", "title for the uploaded video")
	f.IntVar(&chunkMB, "chunk-mb", 8, "maximum chunk size in MiB")
	f.BoolVar(&wait, "wait", false, "block until Meta finishes encoding")
	f.DurationVar(&waitTimeout, "wait-timeout", 15*time.Minute, "how long to wait for encoding")
	f.BoolVar(&quiet, "quiet", false, "suppress upload progress on stderr")

	var statusTimeout time.Duration
	status := &cobra.Command{
		Use:          "status <video-id>",
		Short:        "Wait for a video to finish encoding",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := upload.WaitForVideo(cmd.Context(), rt.Client, args[0], statusTimeout)
			if err != nil {
				return err
			}
			return rt.RenderValue(st)
		},
	}
	status.Flags().DurationVar(&statusTimeout, "timeout", 15*time.Minute, "how long to wait")

	cmd.AddCommand(up, status)
	return cmd
}

// S3Command mints presigned URLs.
//
// Handing Meta a URL to fetch is far cheaper than streaming a large video
// through this machine, and a presigned URL is how you do that for a private
// bucket.
func S3Command(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "s3",
		Short:        "Work with S3-hosted assets",
		GroupID:      "core",
		SilenceUsage: true,
	}

	var ttl time.Duration
	presign := &cobra.Command{
		Use:   "presign <s3://bucket/key>",
		Short: "Generate a temporary public URL for an S3 object",
		Long: "Returns a presigned GET URL, which Meta can fetch directly.\n\n" +
			"Uses the default AWS credential chain.",
		Example: "  url=$(meta-ads s3 presign s3://assets/ad.mp4)\n" +
			"  meta-ads ad-account create-ad-video --id act_123 --file-url \"$url\"",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := store.ParseRef(args[0])
			if err != nil {
				return err
			}
			url, err := rt.S3.Presign(cmd.Context(), ref, ttl)
			if err != nil {
				return err
			}
			// Bare, so it can be captured in a shell variable directly.
			fmt.Fprintln(rt.Out, url)
			return nil
		},
	}
	presign.Flags().DurationVar(&ttl, "expires-in", time.Hour, "how long the URL stays valid")

	cmd.AddCommand(presign)
	return cmd
}

// account resolves which ad account a media command should act on.
func (r *Runtime) account(override string) (string, error) {
	id := override
	if id == "" {
		id = r.Config.AccountID
	}
	if id == "" {
		return "", fmt.Errorf("no ad account: pass --account-id, or set META_ACCOUNT_ID")
	}
	return config.NormalizeAccountID(id), nil
}
