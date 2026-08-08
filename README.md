# meta-ads-cli

A command line interface to the Meta Marketing API, generated from Meta's own SDK.

309 resources, 1445 operations, every parameter typed and every enum resolved — because
the commands are not written by hand. `tools/gen_command_tree.py` walks the AST of
[facebook-python-business-sdk](https://github.com/facebook/facebook-python-business-sdk),
which Meta generates from its internal API specs, and emits a schema the Go binary embeds.
When Meta ships a new API version, you regenerate rather than catch up.

Built to be scripted against and called by language models: JSON by default, structured
discovery commands, real exit codes, and a `--dry-run` that shows the request without
sending it.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/KudcraftsHQ/meta-ads-cli/main/scripts/install.sh | bash
```

Or grab a tarball from [releases](https://github.com/KudcraftsHQ/meta-ads-cli/releases),
or build it yourself:

```bash
go install github.com/KudcraftsHQ/meta-ads-cli/cmd/meta-ads@latest
```

## Authenticate

The minimum is an access token:

```bash
export META_ACCESS_TOKEN="EAAB..."
export META_ACCOUNT_ID="act_123456"     # optional, becomes the default --id
export META_APP_SECRET="..."            # optional, enables appsecret_proof
export META_API_VERSION="v26.0"         # optional, defaults to the generated version
```

For more than one account, use profiles in `~/.config/meta-ads/config.toml`:

```toml
access_token = "EAAB..."
account_id = "act_111"

[client-b]
access_token = "EAAC..."
account_id = "act_222"
```

```bash
meta-ads --profile client-b insights --level campaign
```

Flags beat environment variables, which beat the config file. Check what resolved, and
that it works:

```bash
meta-ads config show
meta-ads config verify
```

## Find your way around

The API is too large to learn from `--help`, so discovery is a first-class command.

```bash
meta-ads list                              # every resource
meta-ads list ad-account                   # its 115 operations
meta-ads describe campaign update          # parameters, types, accepted values
meta-ads describe ad-account --fields      # what --fields accepts
meta-ads tree -o json                      # the whole catalog, for a program to read
```

`describe` prints the accepted values for constrained parameters, which is usually the
question you actually have:

```
$ meta-ads describe ad-account create-campaign
POST {id}/campaigns
  meta-ads ad-account create-campaign --id <AdAccount id> [flags]

FLAG                     TYPE                                 VALUES
--objective              objective_enum                       APP_INSTALLS BRAND_AWARENESS ... OUTCOME_SALES
--special-ad-categories  list<special_ad_categories_enum>     CREDIT EMPLOYMENT HOUSING ... NONE
--daily-budget           unsigned int
```

## Use it

```bash
# Read an account
meta-ads ad-account get --fields id,name,account_status,currency

# Create a campaign
meta-ads ad-account create-campaign \
  --name "Launch" \
  --objective OUTCOME_SALES \
  --status PAUSED \
  --special-ad-categories '[]' \
  --daily-budget 500000

# Every ad in the account, as one JSON document
meta-ads ad-account get-ads --all --max-items 5000 --fields id,name,status

# ...or streamed a line at a time, which starts printing immediately
meta-ads ad-account get-ads --all -o ndjson | jq -r '.id'

# Anything the generated tree does not cover
meta-ads raw GET act_123/ads --params '{"fields":"id,name","limit":5}'
```

### Reporting

Insights is most of what anyone does with this API, so it gets a hand-written command
with the defaults filled in — a standard metric set, identity columns for the level you
asked for, and `last_30d` when you give no dates:

```bash
meta-ads insights --level campaign --preset last_7d -o table
meta-ads insights --level ad --since 2026-07-01 --until 2026-07-31 --all -o csv > july.csv
meta-ads insights --level adset --breakdowns publisher_platform,age
```

The full edge is still there as `meta-ads ad-account get-insights` if you need a parameter
the shortcut does not expose.

### Media

```bash
meta-ads image upload ./creative.png
meta-ads video upload ./ad.mp4 --wait          # chunked, then waits for encoding
meta-ads video upload s3://assets/ad.mp4 --wait
```

File arguments accept a path, `@path`, `file://`, `https://` or `s3://bucket/key`. S3 uses
the default AWS credential chain.

For a large video, handing Meta a URL beats streaming the bytes through your machine:

```bash
url=$(meta-ads s3 presign s3://assets/ad.mp4)
meta-ads ad-account create-ad-video --file-url "$url"
```

## Flags worth knowing

| Flag | |
|---|---|
| `--dry-run` | Print the request that would be sent, credentials redacted. Nothing goes out. |
| `-o, --output` | `json` (default), `ndjson`, `table`, `csv` |
| `--pretty` | Indent JSON |
| `--all`, `--max-items` | Follow pagination on list responses |
| `--params` | A JSON object merged into the request; explicit flags win |
| `--fields` | Comma-separated field selection (`--select` is an alias) |
| `-v, --verbose` | Log requests and Meta's rate-limit headers to stderr |
| `--max-retries` | How many times to wait out a throttle (default 3) |
| `--profile` | Which config-file profile to use |

Exit codes, so scripts can branch:

| | |
|---|---|
| `0` | success |
| `1` | generic failure |
| `2` | bad parameters |
| `3` | authentication or permission |
| `4` | rate limited |
| `5` | server-side or transient |
| `6` | not found |

Rate limits are waited out rather than fatal: a throttled call backs off exponentially and
retries, because a long paginated sweep across a large account will hit one sooner or later
and failing the whole script for it is worse than waiting.

## Shell completion

```bash
meta-ads completion zsh > "${fpath[1]}/_meta-ads"
meta-ads completion bash > /etc/bash_completion.d/meta-ads
```

Completion knows the enum values for every constrained parameter and the selectable fields
for every resource, both straight out of the generated tree.

## Regenerating the command tree

The generated schemas are committed, so building and contributing needs only Go. Python is
needed only to regenerate, which is when Meta ships a new API version:

```bash
make tree      # downloads the SDK, regenerates internal/tree/schemas
make test
```

CI regenerates on every run and warns when the committed tree has drifted from the SDK.

## How it is put together

```
tools/gen_command_tree.py   walks the Facebook SDK's AST -> schemas
internal/tree/schemas/      meta.json (130 B) + index.json (200 KB) + 309 resource files
internal/tree/              embeds them; resolves one resource per invocation
internal/graph/             HTTP, appsecret_proof, retries, pagination, error mapping
internal/dispatch/          schema -> cobra commands, flag coercion, the CLI surface
internal/upload/ store/     media uploads and S3
```

Two things are worth knowing about the design.

**The tree is never fully loaded.** Registering 1445 commands would mean parsing 2 MB of
JSON on every invocation, including `--help`. Instead the arguments are inspected, the named
resource is looked up in the embedded filesystem, and only that one file is unmarshalled.

**The generator stays in Python.** It parses Python source, and Python's `ast` module is the
right tool for that. It is a build-time script that never ships in the binary.

## Prior art

The idea, and the approach of generating the command surface from Meta's Python SDK, comes
from [radjathaher/meta-ads-cli](https://github.com/radjathaher/meta-ads-cli), written in Rust.
This is an independent implementation in Go, not a port: the tree is regenerated from the
upstream SDK rather than copied, and the schema, the lazy-loading design, the retry and
pagination behaviour, the output formats, profiles and the insights command are new here.

## License

MIT. The generated command tree is derived from
[facebook-python-business-sdk](https://github.com/facebook/facebook-python-business-sdk),
which carries Meta's own license; what is extracted from it — endpoint names, HTTP methods
and parameter types — is a factual description of a public API.
