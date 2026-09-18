# Configuration reference

Maintainer Cockpit reads one strict YAML document. Unknown fields, duplicate
identities, and invalid cross-references fail startup or reload. Validate the
configuration with the same source revision or release you will deploy:

```sh
docker compose run --rm app config-check \
  --config /etc/maintainer-cockpit/config.yaml
```

Each release contains `maintainer-cockpit.schema.json` for editor and
automation support. `config-check` is authoritative for current-schema and
cross-field rules. See [Installation](installation.md) for the complete
validation sequence.

## Root fields

- `external_base_url` is the canonical public origin. It must be an HTTPS
  origin without credentials, query, fragment, or path when login is enabled.
- `github` configures collection credentials, its required collection
  schedule, and optional user login.
- `model_providers` defines optional named remote LLM model endpoints.
- `deployment_admins` contains unique positive numeric GitHub user IDs.
  Authenticated users with those IDs gain admin privileges during login.
- `operations` configures the separately authenticated reload endpoint.
- `collections` is a non-empty list of collection definitions.

Secret values never appear directly in YAML. Every secret reference contains
exactly one of:

```yaml
environment: ENVIRONMENT_VARIABLE_NAME
# or
file: /absolute/path/in/the-container
```

## GitHub collection authentication

`github.authentication.profiles` maps a name to one credential and `owners`
assigns each profile to exactly one canonical repository owner. Every
configured repository owner needs a route. Profiles are attempted in listed
order.

A GitHub App profile has:

```yaml
type: github_app
app_id: 123456
installation_id: 12345678
private_key:
  file: /run/secrets/github-app.pem
```

A fine-grained PAT profile has:

```yaml
type: fine_grained_pat
token:
  environment: MAINTAINER_COCKPIT_GITHUB_TOKEN
```

Classic PATs are not supported. Profile and owner names are unique
case-insensitively. Owner spelling must match the canonical casing used by its
repositories. See [GitHub App setup](github-app.md) for required permissions,
installation, login setup, validation, and rotation.

`github.schedule` is required and controls repository collection globally:

```yaml
github:
  schedule: "0 * * * *" # every hour, in UTC
```

## Maintainer login and authorization

`github.user_auth` enables GitHub OAuth login:

```yaml
user_auth:
  client_id: Iv1.example
  client_secret:
    environment: MAINTAINER_COCKPIT_GITHUB_CLIENT_SECRET
```

Its callback is `<external_base_url>/auth/callback`. User access tokens are
discarded after identity and membership are established.

`deployment_admins` grants deployment administration independently from
collection access. A collection's `authorization` may contain any combination
of:

```yaml
authorization:
  organizations: [prometheus]
  teams: [prometheus/maintainers]
  users: [12345678]
```

Organizations and teams use GitHub names; teams use
`organization/team-slug`. Users and deployment administrators use immutable
numeric GitHub IDs. An authorization block must contain at least one entry.

## Model providers

Provider names use lowercase collection-ID syntax. Each provider contains:

- `type`: `openai` or `ollama`;
- `base_url`: an absolute HTTP or HTTPS URL;
- `model`: a non-empty model identifier of at most 200 characters; and
- optional `api_key`: an environment or file secret reference.

Collections opt in through `model_provider`. `model_fallback_provider` is
optional, must name a different configured provider, and is the only way to
enable fallback. A `feature_correlation` block opts into correlation and requires
a five-field `schedule`. See [Model analysis](model-analysis.md) and
[Feature correlation](feature-correlation.md).

## Collections

Each collection has:

- `id`: required immutable lowercase slug, at most 63 characters;
- `name`: required display name, at most 100 characters;
- `description`: optional text, at most 500 characters;
- `repositories`: non-empty unique canonical `owner/repository` identities;
- optional `discovery`, `quality`, `contribution`, model, and authorization
  settings.

Repository casing must remain consistent across collections. Omitted discovery
defaults to all open pull requests. Per-repository discovery is:

```yaml
discovery:
  prometheus/prometheus:
    mode: all_open
  prometheus/node_exporter:
    mode: search
    query: 'label:"help wanted"'
```

`all_open` must not have a query. `search` requires a GitHub search fragment of
at most 256 characters. The repository must also appear in `repositories`.

## Quality and contribution policy

All policy values are optional; omission uses product defaults documented in
[PR quality](pr-quality.md) and [Contribution context](contribution-context.md).

```yaml
quality:
  description_diff_mismatch:
    min_references: 1
    strong_mismatch_min_references: 3
  broad_additions_only:
    min_files: 8
    min_additions: 500
    max_deletions: 10
contribution:
  schedule: "0 3 * * 1"
  established_merged_prs: 3
  unusual_activity:
    account_age_days: 90
    window_days: 14
    min_repositories: 20
    min_organizations: 3
```

All set values are positive except `max_deletions`, which may be zero.
`strong_mismatch_min_references` cannot be lower than `min_references`.
Omitting `contribution` disables periodic contribution evidence for that
collection.

## Cron schedules

All schedules use standard five-field cron syntax:
`minute hour day-of-month month day-of-week`. They are always evaluated in
UTC; seconds, descriptors such as `@daily`, and timezone prefixes are rejected.
There is no hidden jitter. A match enqueues durable work immediately, while
the existing worker concurrency limits bound API calls.

Never-run enabled targets bootstrap immediately. After a restart, occurrences
missed while the process was down are skipped. Overlapping occurrences
coalesce into one active job, and failed jobs wait for the next cron match or
an explicit manual action. Manual `refresh`, `correlate`, and `reanalyze`
requests remain immediate.

## Operations

`operations.reload_secret` protects `POST /-/reload` and is separate from
GitHub and model credentials. See [Deployment operations](operations.md).
