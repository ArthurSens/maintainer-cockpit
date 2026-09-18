# Contributing

Maintainer Cockpit welcomes focused bug fixes, documentation improvements, and
features that preserve the product boundaries in the
[project overview](README.md).

Before starting substantial work, open or join a GitHub Discussion to confirm
the problem and product boundary. Use an issue for accepted, actionable work.
Do not expand an issue into adjacent roadmap items without agreement.

## Development

Requirements are Go 1.26, Node.js 24, and Docker with Buildx. Fork the
repository and create a branch.

Validate the development configuration and start the server:

```sh
go run ./cmd/maintainer-cockpit config-check \
  --config config/maintainer-cockpit.yaml
go run ./cmd/maintainer-cockpit serve \
  --config config/maintainer-cockpit.yaml \
  --database data/maintainer-cockpit.db
```

Alternatively, build and run the local development container:

```sh
docker compose up --build
```

Run the code and container test suites before opening a pull request:

```sh
make test
make test-container
make docs-check
```

When changing telemetry definitions or templates, run
`make telemetry-generate` and commit the generated output. Start new behavior
with an externally observable test where practical. Keep contributor-controlled
content as text and preserve the project's privacy and read-only GitHub
boundaries.

Run `make docs` after changing human-authored Markdown. It formats those files
and checks local and remote links with the pinned mdox tool.

## Pull requests

- Link the issue and describe user-visible behavior.
- Keep commits and the final diff reviewable.
- Include tests and documentation for changed behavior.
- Call out current configuration-schema, API, or telemetry contract changes.
- For database structure changes, state whether existing databases must be
  backed up and recreated.
- Confirm all required CI checks pass.

Contributions are submitted under the [Apache License 2.0](LICENSE). By
participating, you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md).
Security reports follow [SECURITY.md](SECURITY.md), not the public issue
tracker.
