# Feature correlation

Maintainer Cockpit can use an administrator-configured model to identify open
pull requests that compound into one feature, including work spanning multiple
repositories. Correlation is separate from per-pull-request analysis and is
disabled unless a collection explicitly opts in.

## Configuration

Feature correlation requires an explicit five-field UTC schedule:

```yaml
collections:
  - id: observability
    # Other required collection fields are omitted.
    model_provider: local
    feature_correlation:
      schedule: "0 4 * * *"
```

The collection must name a `model_provider`. If `model_fallback_provider` is
configured, correlation uses that explicit fallback after a primary-provider
failure. It never sends data to an unconfigured provider. Omitting
`feature_correlation` disables this periodic model call.

See [Configuration reference](configuration.md) for the complete collection
schema and [Model analysis](model-analysis.md) for provider setup and explicit
fallback behavior.

Use the administrator command to enqueue and coalesce an immediate run:

```sh
maintainer-cockpit correlate \
  --config /etc/maintainer-cockpit/config.yaml \
  --database /var/lib/maintainer-cockpit/maintainer-cockpit.db \
  --collection observability
```

## Input and output contract

`correlation.v1` receives bounded chunks of at most 200 open collection pull
requests. Each entry includes identity, title, author, at most 20 changed-file
paths, and at most 50 already-collected closing-issue, cross-reference, or
associated-PR records. Commit, review, and review-thread relationships are not
correlation inputs. It does not receive diffs, external URL content, previous
correlation groups, or per-PR analysis conclusions. The application never
merges groups across chunks.

The strict output contains named feature groups, descriptions, known PR member
IDs, and evidence-linked edges. Accepted edge types are:

- `duplicated`;
- `competing_design`;
- `stacked_on_top_of` (directed);
- `related`.

Groups and edges require medium or high confidence. Unknown members, invented
evidence IDs, low confidence, self-edges, duplicate edges, extra fields, and
oversized output are rejected.

Open PRs are shown by default. A known closed or merged PR referenced by
collected GitHub evidence may provide historical context and is hidden behind
an explicit UI disclosure. A PR may participate in more than one feature
group.

## Failure and analysis behavior

A successful run atomically replaces the collection's inferred groups,
removing candidates the latest valid result no longer supports. A provider or
schema failure preserves the last successful groups and records a degraded
status. If collected inputs change while a provider call is running, its stale
response is discarded and fresh correlation work is queued. Maintainer
Cockpit does not expose withdrawn-group history.

Correlation never starts, enqueues, or changes the schedule of
`analysis.v2`. When analysis runs for its own reasons, it receives an
application-generated labeled-edge text representation of the current groups
that contain that PR. The correlation input never receives analysis output,
which prevents an analysis/correlation feedback cycle. Deterministic quality
findings and contribution context do not consume correlation data.

The public collection sidebar exposes a Groups table and accessible graph
dialog. Deployment administrators can inspect correlation queue state, last
success, and failures at `/admin`. OpenTelemetry reports attempts, successes,
provider failures, schema-validation failures, queue depth and age, and last
success without prompt content.
