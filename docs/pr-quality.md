# PR quality concerns

Maintainer Cockpit helps maintainers inspect evidence-backed quality concerns
without claiming to identify AI authorship. Deterministic product-owned rules
and validated model-inferred findings contribute to three transparent display
levels. Quality is independent of author history and contribution context.

## Display levels

There is no aggregate numeric score. Every pull request shows exactly one of:

| Level                                 | Meaning                                       |
|---------------------------------------|-----------------------------------------------|
| `no_concerns` (No concerns detected)  | No validated finding.                         |
| `review_suggested` (Review suggested) | One medium finding, or multiple low findings. |
| `strong_concerns` (Strong concerns)   | At least one validated high-severity finding. |

The level derives only from validated findings. The detail drawer explains
every contributing finding, so the display level is always inspectable.

## v0.1 concern taxonomy

The six identifiers below are the stable v0.1 taxonomy. A measurement is
evidence, not a concern by itself, so every finding is either `rule_derived`
or `model_inferred`.

| Family                      | v0.1 evaluator and provenance                                    | Required evidence and completeness                                                                                                                                                         | Severity and confidence                                                                                                 | Missing or partial evidence                                                                                                                |
|-----------------------------|------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------|
| `unrelated_changes`         | Model (`model_inferred`)                                         | Cited stated scope and changed material that directly demonstrates an unrelated change. A positive finding may use partial input.                                                          | `low`, `medium`, or `high`; model confidence rules below apply.                                                         | Return `not_evaluated` unless the supplied evidence directly demonstrates the concern.                                                     |
| `description_diff_mismatch` | Deterministic rule (`rule_derived`) and model (`model_inferred`) | The rule compares path-like description references with collected changed files. The model cites the stated change and supplied changed material. Positive findings may use partial input. | The rule is `medium`, or `high` for the configured strong mismatch. Model severity follows the global confidence rules. | The rule reports `not_evaluated` without changed files. The model does the same unless supplied evidence directly demonstrates a mismatch. |
| `broad_additions_only`      | Deterministic rule (`rule_derived`)                              | Raw provider additions, deletions, and changed-file count.                                                                                                                                 | `medium`; confidence does not apply.                                                                                    | Report `not_evaluated` when file breadth cannot be measured.                                                                               |
| `missing_expected_tests`    | Model (`model_inferred`)                                         | Complete changed-file inventory plus cited evidence of behavior for which tests are expected.                                                                                              | `low`, `medium`, or `high`; model confidence rules below apply.                                                         | Any missing or partial changed-file inventory produces `not_evaluated`, never a finding based on absence.                                  |
| `internal_contradictions`   | Model (`model_inferred`)                                         | Cited supplied statements or changed material that directly conflict. A positive finding may use partial input.                                                                            | `low`, `medium`, or `high`; model confidence rules below apply.                                                         | Return `not_evaluated` unless the supplied evidence directly demonstrates the contradiction.                                               |
| `unsupported_references`    | Model (`model_inferred`)                                         | A referenced GitHub object and the relevant collected material must both be supplied. Wording states only that the supplied evidence does not support the reference.                       | `low`, `medium`, or `high`; model confidence rules below apply.                                                         | Missing or partial referenced material produces `not_evaluated`. External URL content is never fetched or judged.                          |

All six families are release-critical v0.1 behavior. Only the two declared
deterministic rules run without a model provider; the remaining families are
intentionally model-only, not deferred deterministic work. v0.1 adds no
collectors or additional deterministic heuristics for this taxonomy.

## Deterministic findings

Each deterministic finding records:

- **Rule** — which documented rule produced it.
- **Severity** — `low`, `medium`, or `high` (product-owned, not numeric).
- **Provenance** — `rule_derived`.
- **Evidence** — the measured values or quoted references, linked to their
  GitHub source.
- **Completeness** — `complete` when all required inputs were collected,
  `partial` when the rule ran on degraded inputs.

When a rule cannot run at all (for example the changed file list was not
collected), it is reported as *not evaluated* with a reason instead of
silently producing a judgment.

## Initial deterministic rules

### `description_diff_mismatch`

Fires when the description references files and **none** of them appear in the
diff. File references are path-like tokens with an extension (for example
`internal/parser/lexer.go`) or backticked file names (`lexer.go`); URLs
are never treated as file references. Matching is case-insensitive and accepts
path-suffix and bare file-name matches, so partial references stay quiet.

- Severity: `medium`; escalates to `high` when the description references at
  least `strong_mismatch_min_references` files and none match (a complete
  mismatch backed by several references).
- Evidence: every unmatched reference (linked to the pull request) and the
  collected changed-file count (linked to the files tab).

### `broad_additions_only`

Fires on unusually broad additions-only changes: at least `min_files` changed
files, at least `min_additions` added lines, and at most `max_deletions`
deleted lines.

- Uses raw provider totals and changed-file count.
- Severity: `medium`.
- Evidence: the measured churn (linked to the files tab) and the applied
  thresholds.

## Model-inferred findings

When a collection has a configured model provider, the response contains
exactly one evaluation for each model-permitted family:

- `no_concern` requires complete relevant evidence;
- `finding` contains severity, confidence, completeness, a bounded summary,
  and one or more supplied source IDs;
- `not_evaluated` records that evidence was missing or partial and gives a
  bounded reason.

Only `medium`- and `high`-confidence model findings are accepted. A
high-severity model finding additionally requires complete evidence and high
confidence. Partial model findings are capped at medium severity. Findings for
`missing_expected_tests` and `unsupported_references` always require complete
evidence because both depend on absence. These are structural contracts; the
application does not claim to verify the model's semantic correctness.

Unknown fields, missing or duplicate families, invalid status combinations,
unsupported enum values, oversized text, or invented source IDs invalidate the
quality-evaluation section. Other independently valid response sections are
retained, and the attempt is `partial`; a response with no valid section or an
unavailable provider is `unknown` when there is no prior valid result.
Deterministic quality findings and factual PR data remain available in either
case.

Validated model findings are combined with deterministic findings in one
detail assessment and one display level. A high finding produces
`strong_concerns`; a medium finding or multiple low findings produce
`review_suggested`. Model output can raise the level but cannot hide or
downgrade deterministic findings. The detail drawer labels every finding and
not-evaluated family with its provenance. See
[Model analysis](model-analysis.md) for administrator provider configuration,
request bounds, validation, fallback, caching, and explicit reanalysis.

## Explicit AI disclosure

An explicit AI-assistance statement in the pull-request description (for
example `🤖 Generated with Claude Code`, a `Co-authored-by:` trailer naming an
AI tool, or an “AI-assisted” note) is quoted verbatim as **factual metadata**.
It never contributes to the quality level, and undisclosed AI use is never
inferred — prose that merely mentions AI features produces nothing.

## Tuning thresholds per collection

Administrators can tune every documented threshold per collection. Unset
values keep the product defaults, and the UI identifies local policy values on
every threshold shown in the drawer.

```yaml
collections:
  - id: exporters
    name: Exporter maintenance
    repositories: [prometheus/node_exporter]
    quality:
      description_diff_mismatch:
        min_references: 1                  # default 1
        strong_mismatch_min_references: 3  # default 3
      broad_additions_only:
        min_files: 10                      # default 10
        min_additions: 400                 # default 400
        max_deletions: 10                  # default 10
```

Reloading configuration atomically recomputes stored assessments for every
collection whose policy changed.

## Table and API

- The backlog table shows the quality level as a badge and supports sorting
  (`sort=quality`) and filtering (`quality=no_concerns|review_suggested|strong_concerns`).
- The list API returns a compact `quality` summary per pull request; the
  detail API returns the same combined level and the complete assessment:
  deterministic and model findings with provenance and evidence, families that
  were not evaluated, AI disclosures, and the effective policy with
  `default`/`local` provenance per threshold.
- Completed model analysis also exposes all five model-family evaluations, so
  clients can distinguish `no_concern` from `not_evaluated`.
