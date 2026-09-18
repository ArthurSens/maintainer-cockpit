# Troubleshooting

Start with the exact build revision and its two validation commands:

```sh
maintainer-cockpit version
maintainer-cockpit config-check --config /etc/maintainer-cockpit/config.yaml
maintainer-cockpit setup-check --config /etc/maintainer-cockpit/config.yaml
```

Never share private keys, tokens, session cookies, reload credentials, or
retained model payloads in logs or issues.

## Service does not become ready

Check that the SQLite directory exists and is writable by container user
`65532`, the volume is mounted at `/var/lib/maintainer-cockpit`, and the
database has the current structure. `/-/healthy` only confirms the process;
`/-/ready` confirms SQLite can serve requests.

If startup reports a structurally incompatible schema, preserve the database
as a backup and create a fresh database.

## Configuration is rejected

The YAML decoder is strict. Remove unknown fields, use one YAML document, and
match the current configuration schema. Secret blocks contain exactly one of
`environment` or `file`. Use the JSON Schema for editor help, then use
`config-check` for authoritative cross-field validation.

## GitHub setup fails

Common causes are confusing App ID, Client ID, installation ID, and numeric
user ID; missing selected repositories; owner casing differences; unavailable
secret files inside the container; and extra or missing App permissions.

The required repository permissions are Pull requests read-only and Checks
read-only. Organization or team authorization also needs Members read-only.
See [GitHub App setup](github-app.md) for route-specific diagnostics.

## Login or state-changing actions fail

Confirm that `external_base_url` is the exact public HTTPS origin, the GitHub
callback ends in `/auth/callback`, browser requests reach that same origin, and
the reverse proxy preserves request headers. Secure session cookies are not
sent over plain HTTP. A stale session expires after 24 hours; sign in again.

## Collection data is stale or incomplete

Open `/admin` as a deployment administrator. Inspect queue state, last
successful refresh, discovery/hydration/progress/diff/contribution route
attempts, GitHub rate-limit waits, and recent sanitized errors. A failed or
incomplete refresh keeps additions and updates from batches already published,
but defers removals and downstream work until the generation completes.
Retryable hydration failures resume from the failed 20-PR batch. Use the
`refresh` command for an administrator-forced refresh after repairing
credentials or connectivity.

The drawer's readiness facts come from the last successful scheduled snapshot.
`No check runs` means GitHub returned no Check Runs for the current head; it
does not mean legacy Commit Statuses passed, because those are not collected.

## Model analysis is unknown, stale, or failed

Factual GitHub data remains available without a provider. Check provider
health, endpoint reachability from the container, model name, API-key secret,
response schema errors, current-head diff route state, and queue state in
`/admin`. `partial` means valid response sections were retained while invalid
sections remain retryable. `unknown` means no valid current section is
available; `stale` means a prior valid value is shown while replacement work is
queued or running. Fallback occurs only when the collection explicitly names
`model_fallback_provider`.

## Telemetry is absent

No telemetry is exported by default. Confirm `OTEL_CONFIG_FILE` points to a
mounted declarative configuration, restart after changing process environment,
and inspect telemetry export health in `/admin`. Exporter failure does not make
the dashboard unready. See [Deployment operations](operations.md).

## Container architecture mismatch

The source build targets the local Docker platform. Check `docker version`,
`docker image inspect`, and the builder platform. No versioned image is
available yet.
