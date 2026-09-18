# Documentation

Maintainer Cockpit is a self-hosted dashboard for maintainers working through
large GitHub pull-request backlogs. These guides describe the implemented
behavior in the newest release. Documentation on `main` may include changes
intended for the next release.

## Understand

- [Project overview](../README.md) explains what Maintainer Cockpit does and
  the boundaries it preserves.
- [PR quality concerns](pr-quality.md) defines the evidence-backed quality
  levels and concern families.
- [Contribution context](contribution-context.md) defines collection-specific
  author history and the experimental unusual-activity signal.
- [Feature correlation](feature-correlation.md) explains how related pull
  requests are grouped across repositories.

## Install

- [Installation](installation.md) covers versioned containers and binaries,
  artifact verification, validation, persistence, and production steps.
- [Configure GitHub authentication](github-app.md) covers GitHub App and
  fine-grained PAT credentials, permissions, owner routing, and rotation.
- [Reverse proxy and HTTPS](reverse-proxy.md) provides Caddy and nginx
  examples.

## Configure

- [Configuration reference](configuration.md) documents the strict YAML
  schema and cross-field rules.
- [Model analysis](model-analysis.md) configures optional OpenAI-compatible or
  Ollama-compatible model endpoints.

## Use

Start with the collection table. It exposes factual GitHub attributes,
evidence-backed analysis, contribution context, and related feature groups
without computing a single priority score. GitHub login unlocks private
maintainer workflows such as Important PRs, Mine, goals, snooze, and ignore.

The feature references under [Understand](#understand) define what each signal
means and how incomplete evidence is presented.

## Operate

- [Deployment operations](operations.md) covers probes, reloads, schedules,
  retention, runtime diagnostics, and OpenTelemetry.
- [Generated telemetry reference](generated-telemetry.md) lists the
  application-owned metrics.
- [Troubleshooting](troubleshooting.md) starts from common deployment and data
  symptoms.

The telemetry reference is generated from `telemetry/registry`; do not edit it
by hand.

## Contribute

- [Contributing](../CONTRIBUTING.md) covers development, tests, documentation,
  and pull requests.
- [Support](../SUPPORT.md), [security](../SECURITY.md),
  [governance](../GOVERNANCE.md), [release policy](../RELEASES.md), and the
  [Code of Conduct](../CODE_OF_CONDUCT.md) define the community contracts.
