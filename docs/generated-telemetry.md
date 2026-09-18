<!-- Code generated from telemetry/registry by OpenTelemetry Weaver. DO NOT EDIT. -->

# Generated telemetry reference

Generated from `telemetry/registry` with OpenTelemetry Weaver v0.26.1.
## `gen_ai.client.operation.duration`

GenAI operation duration.

- Instrument: `histogram`
- Unit: `s`
- Attributes: `error.type` `gen_ai.operation.name` `gen_ai.provider.name` `gen_ai.request.model` `gen_ai.response.model` `maintainer_cockpit.collection` `maintainer_cockpit.gen_ai.workload` `maintainer_cockpit.model_provider` `server.address` `server.port`

## `gen_ai.client.token.usage`

Number of input and output tokens used.

- Instrument: `histogram`
- Unit: `{token}`
- Attributes: `gen_ai.operation.name` `gen_ai.provider.name` `gen_ai.request.model` `gen_ai.response.model` `gen_ai.token.type` `maintainer_cockpit.collection` `maintainer_cockpit.gen_ai.workload` `maintainer_cockpit.model_provider` `server.address` `server.port`

## `maintainer_cockpit.analysis.attempts`

Model analysis attempts.

- Instrument: `counter`
- Unit: `{attempt}`
- Attributes: `gen_ai.provider.name` `gen_ai.request.model` `maintainer_cockpit.collection`

## `maintainer_cockpit.analysis.provider_failures`

Model-provider request failures.

- Instrument: `counter`
- Unit: `{failure}`
- Attributes: `gen_ai.provider.name` `gen_ai.request.model` `maintainer_cockpit.collection`

## `maintainer_cockpit.analysis.schema_validation_failures`

Model outputs with one or more sections rejected by the product-owned schema, including partially retained responses.

- Instrument: `counter`
- Unit: `{failure}`
- Attributes: `gen_ai.provider.name` `gen_ai.request.model` `maintainer_cockpit.collection`

## `maintainer_cockpit.analysis.stale`

Analyses whose replacement work is queued or running.

- Instrument: `gauge`
- Unit: `{analysis}`

## `maintainer_cockpit.analysis.successes`

Model analyses whose complete response passed product-schema validation.

- Instrument: `counter`
- Unit: `{analysis}`
- Attributes: `gen_ai.provider.name` `gen_ai.request.model` `maintainer_cockpit.collection`

## `maintainer_cockpit.analysis.unknown`

Analysis attempts unavailable for failures outside provider requests and schema validation.

- Instrument: `counter`
- Unit: `{analysis}`
- Attributes: `gen_ai.provider.name` `gen_ai.request.model` `maintainer_cockpit.collection`

## `maintainer_cockpit.collection.batch.completions`

Hydration batches published successfully.

- Instrument: `counter`
- Unit: `{batch}`
- Attributes: `maintainer_cockpit.repository`

## `maintainer_cockpit.collection.batch.pull_requests`

Pull requests published by a completed hydration batch.

- Instrument: `histogram`
- Unit: `{pull_request}`
- Attributes: `maintainer_cockpit.repository`

## `maintainer_cockpit.collection.batch.retries`

Retryable hydration batch failures.

- Instrument: `counter`
- Unit: `{retry}`
- Attributes: `maintainer_cockpit.repository`

## `maintainer_cockpit.collection.cache.bypasses`

Pull requests refreshed while cache use was deliberately bypassed.

- Instrument: `counter`
- Unit: `{pull_request}`
- Attributes: `maintainer_cockpit.repository`

## `maintainer_cockpit.collection.cache.hits`

Pull requests served from cache after matching complete input fingerprints.

- Instrument: `counter`
- Unit: `{pull_request}`
- Attributes: `maintainer_cockpit.repository`

## `maintainer_cockpit.collection.cache.misses`

Pull requests refreshed because no matching cache entry was available.

- Instrument: `counter`
- Unit: `{pull_request}`
- Attributes: `maintainer_cockpit.repository`

## `maintainer_cockpit.collection.failures`

Repository collection failures.

- Instrument: `counter`
- Unit: `{failure}`
- Attributes: `maintainer_cockpit.repository`

## `maintainer_cockpit.collection.forced_reconciliations`

Administrator-forced full repository reconciliations.

- Instrument: `counter`
- Unit: `{reconciliation}`
- Attributes: `maintainer_cockpit.repository`

## `maintainer_cockpit.collection.generation.age`

Age of an active durable repository refresh generation.

- Instrument: `gauge`
- Unit: `s`
- Attributes: `maintainer_cockpit.repository`

## `maintainer_cockpit.collection.last_success`

Unix timestamp of the last complete repository collection.

- Instrument: `gauge`
- Unit: `s`
- Attributes: `maintainer_cockpit.repository`

## `maintainer_cockpit.correlation.attempts`

Feature correlation attempts.

- Instrument: `counter`
- Unit: `{attempt}`
- Attributes: `gen_ai.provider.name` `gen_ai.request.model` `maintainer_cockpit.collection`

## `maintainer_cockpit.correlation.last_success`

Unix timestamp of the last successful correlation by collection.

- Instrument: `gauge`
- Unit: `s`
- Attributes: `maintainer_cockpit.collection`

## `maintainer_cockpit.correlation.provider_failures`

Model-provider correlation request failures.

- Instrument: `counter`
- Unit: `{failure}`
- Attributes: `gen_ai.provider.name` `gen_ai.request.model` `maintainer_cockpit.collection`

## `maintainer_cockpit.correlation.schema_validation_failures`

Correlation outputs rejected by the product-owned schema.

- Instrument: `counter`
- Unit: `{failure}`
- Attributes: `gen_ai.provider.name` `gen_ai.request.model` `maintainer_cockpit.collection`

## `maintainer_cockpit.correlation.successes`

Successfully validated feature correlations.

- Instrument: `counter`
- Unit: `{correlation}`
- Attributes: `gen_ai.provider.name` `gen_ai.request.model` `maintainer_cockpit.collection`

## `maintainer_cockpit.gen_ai.content.items`

Items in a semantic model request or validated response component.

- Instrument: `histogram`
- Unit: `{item}`
- Attributes: `gen_ai.provider.name` `gen_ai.request.model` `maintainer_cockpit.collection` `maintainer_cockpit.gen_ai.content.component` `maintainer_cockpit.gen_ai.content.direction` `maintainer_cockpit.gen_ai.workload` `maintainer_cockpit.model_provider`

## `maintainer_cockpit.gen_ai.content.size`

UTF-8 bytes in a semantic model request or validated response component.

- Instrument: `histogram`
- Unit: `By`
- Attributes: `gen_ai.provider.name` `gen_ai.request.model` `maintainer_cockpit.collection` `maintainer_cockpit.gen_ai.content.component` `maintainer_cockpit.gen_ai.content.direction` `maintainer_cockpit.gen_ai.workload` `maintainer_cockpit.model_provider`

## `maintainer_cockpit.gen_ai.token.details`

Provider-native cached-input and reasoning-output token subsets.

- Instrument: `histogram`
- Unit: `{token}`
- Attributes: `gen_ai.provider.name` `gen_ai.request.model` `maintainer_cockpit.collection` `maintainer_cockpit.gen_ai.token.detail` `maintainer_cockpit.gen_ai.workload` `maintainer_cockpit.model_provider`

## `maintainer_cockpit.gen_ai.usage.reports`

Availability of input and output token usage in provider responses.

- Instrument: `counter`
- Unit: `{report}`
- Attributes: `gen_ai.provider.name` `gen_ai.request.model` `gen_ai.token.type` `maintainer_cockpit.collection` `maintainer_cockpit.gen_ai.usage.availability` `maintainer_cockpit.gen_ai.workload` `maintainer_cockpit.model_provider`

## `maintainer_cockpit.github.rate_limit_terminal_failures`

GitHub rate-limit responses that exhausted retries or the job budget.

- Instrument: `counter`
- Unit: `{failure}`
- Attributes: `maintainer_cockpit.github.rate_limit.type`

## `maintainer_cockpit.github.rate_limit_waits`

Bounded waits before retrying a GitHub rate-limit response.

- Instrument: `counter`
- Unit: `{wait}`
- Attributes: `maintainer_cockpit.github.rate_limit.type`

## `maintainer_cockpit.github.route.attempts`

Persisted attempts to route a GitHub operation through a credential profile.

- Instrument: `counter`
- Unit: `{attempt}`
- Attributes: `maintainer_cockpit.github.failure.category` `maintainer_cockpit.github.profile.type` `maintainer_cockpit.github.route.operation` `maintainer_cockpit.github.route.outcome`

## `maintainer_cockpit.queue.depth`

Number of queued durable jobs.

- Instrument: `updowncounter`
- Unit: `{job}`
- Attributes: `maintainer_cockpit.queue.type`

## `maintainer_cockpit.queue.oldest_age`

Age of the oldest queued durable job.

- Instrument: `gauge`
- Unit: `s`
- Attributes: `maintainer_cockpit.queue.type`

## `maintainer_cockpit.schedule.next_run`

Unix timestamp of the next configured periodic operation run.

- Instrument: `gauge`
- Unit: `s`
- Attributes: `maintainer_cockpit.collection` `maintainer_cockpit.schedule.operation`
