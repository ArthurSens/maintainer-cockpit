# Model analysis

Maintainer Cockpit can send bounded, already-collected public pull-request
context to an administrator-configured HTTP model endpoint. It does not embed
or launch a model, fetch external URL content, or give the model credentials,
tools, or mutation access.

This document is the administrator reference for configuring and operating
model providers. See [PR quality concerns](pr-quality.md) for how validated
model findings affect the quality level shown to maintainers.

## 1. Create an OpenAI project API key

You must have access to an OpenAI API project that can use the model selected
for Maintainer Cockpit.

1. Open the [OpenAI API platform](https://platform.openai.com/).
2. Select an existing project or create a project dedicated to this
   Maintainer Cockpit deployment.
3. Open the project's **Settings → API Keys** page.
4. Select **Create new secret key**.
5. Give the key a deployment-specific name, such as
   `maintainer-cockpit-production`.
6. Prefer a **Restricted** key. Maintainer Cockpit only sends `POST` requests
   to the Chat Completions endpoint, so grant write access to that endpoint
   and no access to unrelated endpoints. The project must also permit the
   configured model.
7. Create the key and immediately copy it into the deployment's secret store.
   OpenAI shows the complete secret only when it is created.

Use a project key dedicated to this deployment rather than sharing a personal
key. Separate deployments should use separate projects or keys so their usage,
limits, and rotation remain independent.

OpenAI references:
[Managing projects](https://help.openai.com/en/articles/9186755-managing-projects-in-the-api-platform),
[creating an API key](https://help.openai.com/en/articles/4936850-how-to-create-and-use-an-api-key),
and [API key safety](https://help.openai.com/en/articles/5112595-best-practices-for-api-key-safety).

## 2. Protect the key

Do not put the key directly in Maintainer Cockpit YAML, commit it, add it to a
container image, or paste it into logs. Configuration must reference exactly
one external secret source: an environment variable or a mounted file.

For a local evaluation, read the key into an environment variable without
putting the value in shell history:

```sh
read -rsp "OpenAI API key: " MAINTAINER_COCKPIT_OPENAI_API_KEY
echo
export MAINTAINER_COCKPIT_OPENAI_API_KEY
```

For a production deployment, prefer a secret manager or a file readable only
by the deployment administrator and application process. If you create the
file manually, avoid a trailing newline:

```sh
umask 077
read -rsp "OpenAI API key: " OPENAI_KEY
echo
printf '%s' "$OPENAI_KEY" > /secure/path/maintainer-cockpit-openai-key
unset OPENAI_KEY
```

Maintainer Cockpit sends the key only in the provider's HTTP authorization
header. The key is not included in retained model payloads.

## 3. Configure the provider and collection

Add an OpenAI-compatible named model-provider profile and assign it to each
collection that should use model analysis:

```yaml
model_providers:
  openai:
    type: openai
    base_url: https://api.openai.com/v1
    model: gpt-5-mini
    api_key:
      environment: MAINTAINER_COCKPIT_OPENAI_API_KEY

collections:
  - id: exporters
    name: Exporters
    repositories:
      - prometheus/node_exporter
    model_provider: openai
```

The profile name `openai` is local configuration and may be changed. The
`model_provider` value must exactly match that profile name. Collections
without a provider continue to expose factual and deterministic attributes;
their model-derived attributes remain unknown.

`gpt-5-mini` is an example model that supports Chat Completions and structured
outputs. Administrators may select another model that supports both features.
Maintainer Cockpit uses the provider's `/v1/chat/completions` endpoint and
supplies its product-owned JSON schema through `response_format`.

OpenAI references:
[GPT-5 mini](https://developers.openai.com/api/docs/models/gpt-5-mini) and
[Structured Outputs](https://developers.openai.com/api/docs/guides/structured-outputs?api-mode=chat).

To use the mounted-file secret instead, change only the key reference:

```yaml
api_key:
  file: /run/secrets/maintainer-cockpit-openai-key
```

## 4. Make the key available to the process

When running from source, the exported environment variable from step 2 is
inherited automatically:

```sh
go run ./cmd/maintainer-cockpit serve \
  --config config/maintainer-cockpit.yaml \
  --database data/maintainer-cockpit.db
```

For Docker Compose with the environment-variable form, add the variable to the
app service:

```yaml
services:
  app:
    environment:
      MAINTAINER_COCKPIT_OPENAI_API_KEY: ${MAINTAINER_COCKPIT_OPENAI_API_KEY}
```

For Docker Compose with the mounted-file form, add the secret as a read-only
volume:

```yaml
services:
  app:
    volumes:
      - /secure/path/maintainer-cockpit-openai-key:/run/secrets/maintainer-cockpit-openai-key:ro
```

The path inside the container must match `api_key.file` in the application
configuration.

## 5. Validate the configuration

Validate the complete YAML document before restarting the service:

```sh
docker compose run --rm app config-check \
  --config /etc/maintainer-cockpit/config.yaml
```

Or, when running from source:

```sh
go run ./cmd/maintainer-cockpit config-check \
  --config config/maintainer-cockpit.yaml
```

This check verifies the current configuration schema, provider type, URL,
model, collection references, and secret-reference syntax. It does not
contact OpenAI or verify the key. The service resolves the configured secret
at startup and fails before serving if the secret is missing or empty.

## 6. Start and verify analysis

Start or recreate the service after validation:

```sh
docker compose up -d --build
```

New or changed collected pull requests first queue authenticated current-head
diff collection. Analysis is queued after that attempt is complete, partially
bounded, or unavailable, so core PR metadata remains assessable during GitHub
transport or authentication failures. Unavailable diff evidence is retried by
later scheduled repository refreshes.
Results created with an older product-owned analysis schema are also
reanalyzed when queued. If pull requests were already collected before the
provider was configured, request collection-wide reanalysis:

```sh
docker compose run --rm app reanalyze \
  --config /etc/maintainer-cockpit/config.yaml \
  --database /var/lib/maintainer-cockpit/maintainer-cockpit.db \
  --collection exporters
```

Open the collection dashboard and inspect a pull request. A completed analysis
shows Review Cognitive Load, waiting-on states, model-inferred quality findings,
provider, model, analysis time, and evidence source IDs.

Non-sensitive queue health is also available at:

```sh
curl --fail http://127.0.0.1:8765/api/analysis/status
```

The current implementation has no separate provider connectivity check.
Authentication, model-access, quota, rate-limit, and schema errors become
visible analysis failures while factual pull-request data remains available.

## 7. Rotate or revoke the key

Create a replacement key before revoking the current key.

- **Environment variable:** update the variable and recreate the app container
  so the process receives the new value.
- **Mounted file:** replace the file atomically, then send `SIGHUP` so
  Maintainer Cockpit reloads configuration and secret material.

For the environment-variable form:

```sh
docker compose up -d --force-recreate app
```

For the mounted-file form:

```sh
docker compose kill --signal SIGHUP app
```

Request reanalysis for any pull requests whose attempts failed during
rotation. Revoke the old key in the OpenAI project only after the service is
successfully using the replacement.

## Troubleshooting

- **Secret environment variable is empty or unset:** Export the exact variable
  named by `api_key.environment`, and pass it into the app container.
- **Secret file cannot be opened:** Confirm that the host file is mounted
  read-only at the exact path named by `api_key.file`.
- **HTTP 401:** The key is invalid, revoked, or belongs to a project that
  cannot authenticate the request. Replace the key and recreate or reload the
  service.
- **HTTP 403 or model access failure:** Confirm that the key's project can use
  the configured model and that a restricted key can write to Chat
  Completions.
- **HTTP 429:** Inspect the OpenAI project's usage and rate limits, then retry
  with `reanalyze` after capacity is available.
- **Invalid model response:** Select a model that supports Structured Outputs.
  Maintainer Cockpit rejects output that does not match its schema.
- **Analysis remains unknown:** Confirm that the collection names the provider,
  GitHub collection has completed, and an analysis job has been queued.

## Bounded input and validation

Each request is deterministic and contains at most:

- 12 KiB of the PR description;
- 100 changed-file paths;
- 300 evidence sources overall;
- 2 KiB of text per non-diff evidence source;
- 100 authenticated per-file diff sources, with 8 KiB per file and 128 KiB
  across all patches.

The request records diff completeness plus original, sent, omitted, and
truncated file/byte metadata. `original_bytes` counts patch bytes GitHub
returned for the inspected, bounded file set; GitHub does not expose byte totals
for omitted files or files whose patch is omitted. Files retain GitHub's returned
order. The collector verifies the PR head immediately before and after listing
files, so evidence is never cached across a force-push boundary. Diff sources
use stable `diff:<path>` IDs and are cached by repository, pull-request number,
and head SHA. Inputs include factual
PR metadata, raw additions and deletions, file paths, and one-hop GitHub
relationships already present in SQLite. External URL content and product or
provider credentials are not input fields. If the PR belongs to current
validated feature groups, analysis also receives their application-generated
labeled-edge text. Correlation never enqueues analysis; the text is included
only when an event-driven analysis job runs.

Responses must contain the three product-owned root sections
`reviewCognitiveLoad`, `waitingOn`, and `qualityEvaluations`.
Maintainer Cockpit validates those sections independently, rejecting unknown
fields, enum values, duplicate waiting parties, oversized text, and citations
to source IDs absent from the request. A valid section is retained even when
another section is invalid. The latest attempt is then `partial` and remains
eligible for retry.

Quality output contains exactly one evaluation for each model-permitted v0.1
concern family. [PR quality concerns](pr-quality.md) is the canonical contract
for those families, evidence requirements, confidence, completeness, and
degraded behavior.

## Review Cognitive Load

The provider supplies an evidence-linked overall review-effort assessment plus
change scope, required context, conceptual complexity, and review risk
components. Components may be unknown when evidence is insufficient, while the
overall assessment remains low, medium, or high and explains any partial
evidence.

Waiting-on is a set and may contain triager, maintainer, author, and external
dependency simultaneously.

## Caching and provenance

The latest attempt status and error are stored separately from last-valid
analysis sections. Provider and wholly invalid response failures therefore
preserve prior valid output; the first such failure has no Review Cognitive
Load result. Partial attempts merge valid new sections with prior valid
sections and remain retryable. Public status distinguishes `partial` from
`unknown` (no current valid output) and `stale` (prior valid output while
replacement work is queued or running).

Only a full successful result is cached by a deterministic revision of factual
model input. Changes to the focal PR or included related evidence enqueue new
analysis. Analysis-schema changes make an otherwise matching cached result
stale when it is next queued. Prompt-version changes alone do not invalidate
the cache. The current provenance identifiers are schema `analysis.v2` and
prompt `analysis-prompt.v2`.

The API exposes analysis state, evidence, provider, model, analysis time,
input revision, prompt/schema versions, and an opaque retained-payload ID.
Raw retained payloads remain separate and restricted to deployment administrators.

Explicit reanalysis follows step 6. Duplicate active requests are coalesced.
