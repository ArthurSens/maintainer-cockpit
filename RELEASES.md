# Release policy

Maintainer Cockpit follows Semantic Versioning. The current `main` branch is
development for the next release. v0.0.1 is the first supported release and
the first version with stable deployment artifacts.

Before 1.0, only the newest published release is supported and any minor or
patch release may contain documented breaking changes.

Every breaking configuration, browser API, database, or telemetry change and
its deployment impact must be listed in that version's release notes.

Version tags publish Linux `amd64` and `arm64` binaries, a multi-platform
container image, checksums, an SPDX SBOM, configuration schema, and release
notes. Images are signed keylessly with
Cosign and include BuildKit provenance and SBOM attestations. The immutable
version tag is the recommended deployment reference. The project does not
publish a mutable `latest` tag.

Release candidates must pass the repository's Go, browser, generated-artifact,
and multi-platform container checks. Declaring `v0.1.0` ready also requires at
least one maintainer other than the project author to install and use the
release from the documentation.
