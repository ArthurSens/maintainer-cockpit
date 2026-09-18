# Configure GitHub authentication

Maintainer Cockpit routes each repository owner through an ordered list of
named collection credentials. A credential is either a GitHub App installation
or a fine-grained personal access token (PAT). Each profile belongs to exactly
one owner. GitHub App profiles are preferred because they use short-lived
installation tokens; fine-grained PATs can be configured as primary or backup
routes. Classic PATs are not supported.

The separate `github.user_auth` GitHub App OAuth flow authenticates maintainers.
User tokens are used only during login to establish numeric identity and
membership, then discarded. Collection credentials are scheduled backend
credentials and do not depend on a maintainer session. Webhooks are not used.

These instructions match the current product capabilities. GitHub's general
registration guide includes options that Maintainer Cockpit does not use yet.

## 1. Register an app

You must be able to create a GitHub App for the personal account or
organization that will own it.

1. Open the GitHub App registration page:
   - Personal account: **Settings → Developer settings → GitHub Apps → New
     GitHub App**
   - Organization: **Organization settings → Developer settings → GitHub Apps
     → New GitHub App**
2. Enter a unique app name.
3. Set **Homepage URL** to the deployed Maintainer Cockpit URL. For a local
   evaluation, GitHub also permits the URL of the source repository or the
   account that owns the app.
4. Set **Callback URL** to the canonical deployment URL followed by
   `/auth/callback`, for example
   `https://cockpit.example.org/auth/callback`. Leave **Setup URL** empty.
5. Do not select **Request user authorization (OAuth) during installation**.
6. Under **Webhook**, clear **Active**. Maintainer Cockpit refreshes on a
   schedule and does not receive webhook events.
7. Under **Repository permissions**, set:
   - **Pull requests:** Read-only
   - **Checks:** Read-only
   - **Organization permissions → Members:** Read-only when any collection
     authorizes an organization or team.
   - Every other selectable permission: No access

   GitHub grants read-only repository metadata access with every installation;
   it does not need to be configured separately.
8. Do not subscribe to events.
9. Under **Where can this GitHub App be installed?**, choose:
   - **Only on this account** when the app owner also owns all target
     repositories.
   - **Any account** when another account or organization must install it.
10. Create the app.

On the new app's settings page, copy the numeric **App ID**. The App ID and
Client ID are different; Maintainer Cockpit requires the App ID.

After creating the app, also copy its **Client ID** and generate a **Client
secret**. The Client ID differs from the numeric App ID. Store the client
secret with the same care as the private key.

GitHub references:
[Registering a GitHub App](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app).
[Generating a user access token for a GitHub App](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app).

## 2. Generate and protect a private key

1. On the app's settings page, scroll to **Private keys**.
2. Select **Generate a private key**.
3. Store the downloaded `.pem` file in a secret store or another location that
   is readable only by the deployment administrator and application process.

Do not commit the key, copy it into the YAML configuration, or put it in a
container image. GitHub stores only the public half, so keep the downloaded
private key safe. A GitHub App private key can be used to request access for
every account where that app is installed.

GitHub reference:
[Managing private keys for GitHub Apps](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/managing-private-keys-for-github-apps).

## 3. Install the app for one owner

1. On the app's settings page, select **Install App**.
2. Select **Install** next to the personal account or organization that owns
   the target repositories.
3. Prefer **Only select repositories**, then select every configured repository
   for this owner that will use the profile.
4. Confirm the installation.

Copy the numeric installation ID from the installation page URL:

```text
https://github.com/organizations/ORGANIZATION/settings/installations/INSTALLATION_ID
```

For a personal installation, the URL uses
`https://github.com/settings/installations/INSTALLATION_ID`.

GitHub also documents discovering this value through its REST API using
`GET /app/installations`, `GET /orgs/{org}/installation`, or
`GET /repos/{owner}/{repo}/installation`. Those endpoints require
GitHub App JWT authentication; reading the installation page URL is simpler
for manual setup.

GitHub references:
[Installing your own GitHub App](https://docs.github.com/en/apps/using-github-apps/installing-your-own-github-app)
and
[Authenticating as a GitHub App installation](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/authenticating-as-a-github-app-installation).

## 4. Configure Maintainer Cockpit

The current configuration uses typed profiles and owner routes and requires a
collection schedule. Owner keys must use the same canonical casing as their
configured repositories, and order in `profiles` defines primary then backup
precedence:

```yaml
external_base_url: https://cockpit.example.org

github:
  schedule: "0 * * * *"
  authentication:
    profiles:
      prometheus-app:
        type: github_app
        app_id: 12345
        installation_id: 67890
        private_key:
          file: /run/secrets/maintainer-cockpit-github-app.pem
      prometheus-pat:
        type: fine_grained_pat
        token:
          environment: MAINTAINER_COCKPIT_PROMETHEUS_PAT
    owners:
      prometheus:
        profiles:
          - prometheus-app
          - prometheus-pat
  user_auth:
    client_id: Iv1.example
    client_secret:
      environment: MAINTAINER_COCKPIT_GITHUB_CLIENT_SECRET

deployment_admins:
  - 12345678

collections:
  - id: prometheus
    name: Prometheus
    repositories:
      - prometheus/prometheus
      - prometheus/node_exporter
    authorization:
      organizations:
        - prometheus
      teams:
        - prometheus/maintainers
      users:
        - 87654321
```

Organization and team checks use the app's read-only **Members** permission.
Configured parent teams include members inherited from child teams. Individual
users and deployment administrators are configured by immutable numeric GitHub
user ID; collection authorization does not make a user a deployment
administrator.

Each `private_key` or `token` block must contain exactly one external secret
source:

```yaml
# Mounted secret file, recommended for deployments.
private_key:
  file: /run/secrets/maintainer-cockpit-github-app.pem
```

```yaml
# Environment variable, convenient for local evaluation.
token:
  environment: MAINTAINER_COCKPIT_PROMETHEUS_PAT
```

For the mounted-file form, make the PEM available to the container as a
read-only volume. For example, add this entry to the app service's `volumes`:

```yaml
- /secure/path/maintainer-cockpit.pem:/run/secrets/maintainer-cockpit-github-app.pem:ro
```

For a local Docker Compose evaluation using the environment-variable form:

```sh
export MAINTAINER_COCKPIT_GITHUB_PRIVATE_KEY="$(
  cat /secure/path/maintainer-cockpit.pem
)"
```

The included `compose.yaml` passes its configured variables into the app
container. Configure the OAuth client secret through its own environment
variable or mounted secret file. Collection authentication and `user_auth`
remain separate; neither secret is retained in sessions.

## 5. Validate the connection

Validate the complete setup before starting the service:

```sh
docker compose run --rm app setup-check \
  --config /etc/maintainer-cockpit/config.yaml
```

Or, when running from source:

```sh
go run ./cmd/maintainer-cockpit setup-check \
  --config config/maintainer-cockpit.yaml
```

A setup check resolves environment- and file-backed secrets and probes every
configured profile for every repository on its owner route. For an App profile
it validates the private key, App and installation identity, owner, repository
grant, required permissions, and token issuance. For a PAT profile it probes
the target repository through REST and authenticated GraphQL.

The first profile is the required primary route: any primary failure makes the
command fail. A lower-priority backup failure is printed as a warning so an
unused broken backup is still visible. Checks are aggregate where practical
and never print secret values.

The check does not change repositories.

## 6. Start collection

Start the service after `setup-check` succeeds:

```sh
docker compose up --build
```

The service queues repository refreshes on the global five-field UTC
`github.schedule`. To request an immediate administrator-forced refresh that
bypasses the pull-request cache:

```sh
docker compose run --rm app refresh \
  --config /etc/maintainer-cockpit/config.yaml \
  --database /var/lib/maintainer-cockpit/maintainer-cockpit.db
```

Every scheduled run re-discovers open PRs. An unchanged PR produces a cache hit
when the persisted `github-input-v1` fingerprint still covers the same focal
and related inputs.

GitHub `403` and `429` primary and secondary rate limits are retried within a
bounded job budget. The client honors `Retry-After`,
`X-RateLimit-Remaining`, and `X-RateLimit-Reset`; cancellation interrupts a
wait. If the required wait exceeds the budget or retries are exhausted, the
job fails and the previous complete snapshot remains visible.

Each discovery, hydration batch, progress, current-head pull-request-diff, or
per-author/per-repository
contribution-evidence operation starts again at the owner's first profile.
Before an App route advances after an authentication failure, Maintainer
Cockpit evicts and refreshes its installation token once.
It advances only for bounded route, authorization, or capability failures,
including a missing/suspended installation, missing repository grant or
permission, invalid credential, or missing capability. It does not advance on
rate limits, transport failures, GitHub 5xx responses, cancellation, or
response-decode failures.

Contribution evidence runs asynchronously from core repository refreshes.
Bounded ancillary author calls, such as public account/activity facts, are
unauthenticated work and are not configurable public profiles.

When a backup succeeds, the current route warning identifies the affected
repository and operation. A later operation retries the primary; success on
that primary replaces the operation's attempt set and clears its warning.
Failed refreshes retain the last complete repository snapshot.

## Route diagnostics and telemetry

Maintainers see sanitized current warnings with repository, operation,
fallback/failed/pending status, freshness, and timestamps. Profile names,
installation IDs, credential locations, and raw GitHub errors are omitted.
Deployment Admin status includes the ordered attempts, profile name and type,
bounded failure category, short detail, and remediation.

The route-attempt counter uses only bounded operation (`discovery`, `hydration`,
`progress`, `contribution_evidence`, or `pull_request_diff`), profile type,
outcome, and failure-category attributes.
Profile names, owners, repositories,
App/installation/account IDs, secret locations, and arbitrary error text are
not route-metric dimensions.

## Change repositories or rotate the key

When adding a repository:

1. Open the installed GitHub App's **Configure** page.
2. Add the repository under **Repository access** and save.
3. Add the same canonical repository to Maintainer Cockpit configuration.
4. Send `SIGHUP` to reload the validated configuration.

GitHub reference:
[Reviewing and modifying installed GitHub Apps](https://docs.github.com/en/apps/using-github-apps/reviewing-and-modifying-installed-github-apps).

To rotate a private key without downtime, generate a replacement before
deleting the old key. With a mounted file, replace the file atomically and send
`SIGHUP`. With an environment variable, run `setup-check` in a new Compose
container and then recreate the app container so it receives the new
environment:

```sh
docker compose run --rm app setup-check \
  --config /etc/maintainer-cockpit/config.yaml
docker compose up -d --force-recreate app
```

Delete the old key from GitHub only after validation succeeds and the service
is running with the replacement.

Reload builds and validates replacement configuration and credentials before
publishing them. Invalid configuration or unavailable secrets reject the
reload and retain the prior runtime configuration. Every authenticated route
is marked pending and revalidated asynchronously so credentials rotated behind
an unchanged secret reference are also checked; existing last-complete
snapshots remain visible while that work runs or if it fails.

## Troubleshooting

- **App ID mismatch:** Use the numeric App ID from the app settings page, not
  the Client ID.
- **Installation mismatch:** Use the installation ID from the installation's
  configuration URL, not the App ID or an organization ID.
- **Primary route failed:** The first profile is required even when a backup
  validates. Repair it or intentionally reorder the owner's profile list.
- **Backup route warning:** Repair the lower-priority profile before it is
  needed; its warning does not by itself fail `setup-check`.
- **PAT rejected:** Configure a fine-grained PAT with the target repository and
  required read permissions. Classic PATs are outside the supported contract.
- **Permission validation failed:** Keep only `Pull requests: Read-only` and
  `Checks: Read-only`.
  Maintainer Cockpit deliberately rejects write and unrelated permissions.
- **Repository token validation failed:** Confirm that every configured
  repository is selected in the installation and spelled as
  `owner/repository`.
- **Organization or team login denied:** Confirm **Members: Read-only** is
  enabled and that the app is permitted by the organization.
- **OAuth callback rejected:** Confirm `external_base_url` is the exact HTTPS
  deployment origin and the GitHub App callback is
  `<external_base_url>/auth/callback`.
- **Private key parsing failed:** Confirm that the secret contains the complete
  downloaded PEM, including its `BEGIN` and `END` lines.
- **Secret is empty in Compose:** Export the environment variable in the shell
  that runs `docker compose`, or use a mounted secret file.
