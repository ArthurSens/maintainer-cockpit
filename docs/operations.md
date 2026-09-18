# Deployment operations

Maintainer Cockpit exposes two public, detail-free probes:

- `GET /-/healthy` reports that the HTTP process is running.
- `GET /-/ready` reports whether SQLite can serve requests.

Model-provider, GitHub, and telemetry-exporter outages do not make the factual
dashboard unready. Their failures remain visible to deployment administrators
at `/admin`.

## Runtime profiling

The HTTP listener always exposes Go runtime profiles below `/debug/pprof/`.
For example, capture a 30-second CPU profile with:

```sh
go tool pprof http://127.0.0.1:8765/debug/pprof/profile?seconds=30
```

Profiles expose internal process details. Keep the listener on a trusted
network and restrict `/debug/pprof/` at the reverse proxy before making the
application reachable from an untrusted network.

## Reload

The reload credential is separate from GitHub login and all provider
credentials:

```yaml
operations:
  reload_secret:
    environment: MAINTAINER_COCKPIT_RELOAD_SECRET
```

Reload the complete runtime configuration with:

```sh
curl --fail --request POST \
  --header "Authorization: Bearer ${MAINTAINER_COCKPIT_RELOAD_SECRET}" \
  https://cockpit.example.test/-/reload
```

The application validates configuration, resolves credentials, and constructs
replacement GitHub and model clients before publishing the new configuration.
`SIGHUP` uses the same runtime reload path. After a successful reload, every
authenticated repository route is marked pending and queued for revalidation.
Existing last-complete snapshots remain visible while revalidation runs or if
it fails.

## Schedules, forced refresh, and GitHub limits

The `/admin` status page reports each enabled UTC cron expression and the time
until its next run. Its API returns the same exact next-run timestamp.
OpenTelemetry exports that timestamp through
`maintainer_cockpit.schedule.next_run`.

The page also reports each repository's last cache hits, misses, and bypasses,
whether the last run was administrator-forced, and the latest bounded GitHub
rate-limit wait or terminal failure. The `refresh` command bypasses the cache;
duplicate queued requests are coalesced and upgraded to forced work.

Repository refreshes discover the complete open set, hydrate at most 20 pull
requests per durable batch, and publish each completed batch immediately.
Additions and updates become visible at batch completion, while removals,
pull-request diff work, and analysis wait until incremental progress and the
full generation finish. Completed batches survive process restarts and
retryable GitHub failures. Correlation waits while any repository in its
collection has active refresh work.

OpenTelemetry exposes cache hits, misses, bypasses, forced reconciliation,
batch completions, batch sizes, batch retries, active-generation age,
primary/secondary rate-limit waits, and terminal failures without PR numbers,
user identities, or other high-cardinality attributes.

The same administrator page reports feature-correlation queue depth, failures,
and the latest successful run. Use `correlate --collection <id>` to enqueue an
immediate run; active duplicate work is coalesced. Correlation provider and
schema failures leave the last successful public groups intact.

## Retention and audit

At startup and daily, the process removes completed refresh generations,
analysis and correlation jobs, retained model payloads, and personal progress
older than three months. Removed collections are hidden immediately and
retained for seven days so an accidental configuration removal can be reversed
by restoring the same immutable collection ID.

Audit events contain only an event name, time, optional numeric GitHub user ID,
and a bounded product-defined detail code. Tokens, model requests, model
responses, and prompt content are rejected from audit details.

## OpenTelemetry

The application exports nothing by default. Set `OTEL_CONFIG_FILE` to an
OpenTelemetry declarative configuration file to independently configure trace
and metric exporters. Invalid telemetry configuration fails startup; exporter
outages degrade telemetry status but do not affect probes or factual pages.

### Local Grafana LGTM example

For development and testing, `compose.otel-lgtm.yaml` adds the
[`grafana/otel-lgtm`](https://grafana.com/docs/opentelemetry/docker-lgtm/)
image as an OTLP receiver and mounts `examples/otel-lgtm/otel-sdk.yaml` into
the application. Start the application and observability stack together:

```sh
docker compose \
  -f compose.yaml \
  -f compose.otel-lgtm.yaml \
  up --build
```

The example SDK configuration exports traces and metrics over OTLP/HTTP every
five seconds. Open Grafana at http://localhost:3000, sign in with username
`admin` and password `admin`, then open the **Maintainer Cockpit** folder and
the **Maintainer Cockpit — GenAI Operations** dashboard. The dashboard provides
an on-call overview of:

- analysis volume, success ratio, and provider failures;
- GenAI request rate, error rate, latency, tokens, and error types;
- token spend by analysis or correlation workload and configured provider profile;
- provider-native cached-input and reasoning-output token subsets;
- request and validated-response component sizes, plus token-reporting coverage; and
- queue depth, oldest-job age, and collection freshness.

Use the collection, workload, provider profile, provider kind, and model variables
to narrow an investigation. Provider-reported aggregate input and output tokens
are the source of truth for usage. Cached-input and reasoning-output values are
subsets of those totals and must not be added to them.

Component panels report exact UTF-8 content bytes and item counts for bounded
semantic categories such as system instructions, output schemas, descriptions,
files, non-diff evidence, diff evidence, relationships, Review Cognitive Load,
summaries, and evaluations. They explain why a request or response is large,
but they are not token estimates: tokenization is model-specific and providers
may add framing that the application cannot attribute to one component.
Missing provider usage fields are reported as missing rather than as zero
tokens.

For an individual expensive call, use its GenAI trace. Analysis spans include
the repository, pull request, and durable job ID; correlation spans include the
job and chunk position. The correlated structured success log also contains the
provider-reported token totals and provider-native details.
Explore remains available for ad hoc queries with the preconfigured Tempo and
Prometheus data sources. The OTLP/HTTP and OTLP/gRPC receivers are also
available to host processes at `localhost:4318` and `localhost:4317`.

The dashboard JSON is generated by Weaver from
`telemetry/templates/registry/grafana`; do not edit the generated JSON directly.
Run `make telemetry-generate` after changing its template or the telemetry
registry. CI verifies that the committed dashboard matches the generator.

Stop the example without deleting its telemetry volume:

```sh
docker compose \
  -f compose.yaml \
  -f compose.otel-lgtm.yaml \
  down
```

The LGTM image is intended only for development, demonstrations, and testing;
it is not a production deployment architecture.

The implementation uses `go.opentelemetry.io/contrib/otelconf` and pins the
Development GenAI semantic conventions to OpenTelemetry semantic-conventions
release `v1.44.0`. Application-owned metrics are defined in
`telemetry/registry`; upstream GenAI definitions are imported through its
pinned registry manifest and artifacts are generated with OpenTelemetry Weaver
`v0.26.1`. The [generated telemetry reference](generated-telemetry.md) lists
every application-owned metric and attribute.

Generated telemetry includes numeric token counts and semantic content sizes,
but never prompt text, response text, credential tokens, payload content, user
IDs, PR titles, or error text. Repository and pull request identity appear only
on traces and correlated logs, not on Prometheus metrics. Configured provider
profile names appear on GenAI measurements so operators can distinguish
multiple endpoints or accounts of the same provider kind.
