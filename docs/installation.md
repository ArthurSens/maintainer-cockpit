# Installation

Maintainer Cockpit v0.0.1 is available as a multi-platform container image and
as Linux `amd64` and `arm64` binary archives. Use the immutable version tag
rather than a mutable image tag or `main`.

## Prerequisites

- Docker Engine for the recommended container installation, or a supported
  Linux host for the binary;
- a public HTTPS origin for GitHub login; and
- a GitHub App or fine-grained PAT that can read every configured repository.

## 1. Get the configuration

Create a deployment directory and download the example configuration from the
same version you will run:

```sh
mkdir maintainer-cockpit-v0.0.1
cd maintainer-cockpit-v0.0.1
curl --fail --location --output maintainer-cockpit.yaml \
  https://raw.githubusercontent.com/ArthurSens/maintainer-cockpit/v0.0.1/config/maintainer-cockpit.yaml
```

Replace every example owner, repository, ID, URL, and secret reference. Follow
[Configure GitHub authentication](github-app.md) to create credentials with
the required read-only permissions.

Secrets are not accepted directly in YAML. Reference an environment variable
or a file mounted into the container:

```yaml
private_key:
  environment: MAINTAINER_COCKPIT_GITHUB_PRIVATE_KEY
```

For a small deployment, export each referenced environment variable in the
shell that starts the container. For production, prefer read-only files
supplied by a secret manager.

## 2. Pull and validate the container

Pull the immutable image:

```sh
export IMAGE=ghcr.io/arthursens/maintainer-cockpit:v0.0.1
docker pull "${IMAGE}"
```

Validate the YAML without resolving secrets or contacting GitHub:

```sh
docker run --rm \
  --mount type=bind,src="$(pwd)/maintainer-cockpit.yaml",dst=/etc/maintainer-cockpit/config.yaml,readonly \
  "${IMAGE}" config-check --config /etc/maintainer-cockpit/config.yaml
```

Then forward the configured secrets and verify every repository route:

```sh
docker run --rm \
  --env MAINTAINER_COCKPIT_GITHUB_PRIVATE_KEY \
  --env MAINTAINER_COCKPIT_GITHUB_TOKEN \
  --env MAINTAINER_COCKPIT_GITHUB_CLIENT_SECRET \
  --mount type=bind,src="$(pwd)/maintainer-cockpit.yaml",dst=/etc/maintainer-cockpit/config.yaml,readonly \
  "${IMAGE}" setup-check --config /etc/maintainer-cockpit/config.yaml
```

`setup-check` is read-only. A failure in an owner's first credential profile is
an error; a failure in a lower-priority backup is a warning.

## 3. Start the service

Create the persistent data volume and start the application:

```sh
docker volume create maintainer-cockpit-data
docker run --detach \
  --name maintainer-cockpit \
  --restart unless-stopped \
  --publish 127.0.0.1:8765:8765 \
  --env MAINTAINER_COCKPIT_GITHUB_PRIVATE_KEY \
  --env MAINTAINER_COCKPIT_GITHUB_TOKEN \
  --env MAINTAINER_COCKPIT_GITHUB_CLIENT_SECRET \
  --env MAINTAINER_COCKPIT_RELOAD_SECRET \
  --mount type=bind,src="$(pwd)/maintainer-cockpit.yaml",dst=/etc/maintainer-cockpit/config.yaml,readonly \
  --mount type=volume,src=maintainer-cockpit-data,dst=/var/lib/maintainer-cockpit \
  "${IMAGE}"
```

Verify the process and database:

```sh
curl --fail http://127.0.0.1:8765/-/healthy
curl --fail http://127.0.0.1:8765/-/ready
```

Both endpoints return `{"status":"ok"}` when healthy.

## 4. Put HTTPS in front

Keep the application port private. Follow
[Reverse proxy and HTTPS](reverse-proxy.md), set `external_base_url` to the
exact public HTTPS origin, and configure the GitHub App callback as
`<external_base_url>/auth/callback`.

Do not enable GitHub login over plain HTTP. Sessions use Secure cookies and
state-changing browser requests validate their origin.

## Verify release artifacts

Each GitHub release contains checksums, a keyless Sigstore bundle, an SPDX
SBOM, the configuration schema, and the image digest. Download the checksum
files from the GitHub release page for v0.0.1, then verify their signing
identity:

```sh
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity \
    "https://github.com/ArthurSens/maintainer-cockpit/.github/workflows/publish.yaml@refs/tags/v0.0.1" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  checksums.txt
sha256sum --check checksums.txt --ignore-missing
```

The signed image digest is in `image-digest.txt`. Verify the image itself with:

```sh
cosign verify \
  --certificate-identity \
    "https://github.com/ArthurSens/maintainer-cockpit/.github/workflows/publish.yaml@refs/tags/v0.0.1" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  "$(cat image-digest.txt)"
```

## Install the Linux binary

Download the archive for the host architecture from the GitHub release page
for v0.0.1. For `amd64`:

```sh
curl --fail --location --remote-name \
  https://github.com/ArthurSens/maintainer-cockpit/releases/download/v0.0.1/maintainer-cockpit_v0.0.1_linux_amd64.tar.gz
tar -xzf maintainer-cockpit_v0.0.1_linux_amd64.tar.gz
install -m 0755 \
  maintainer-cockpit_v0.0.1_linux_amd64/maintainer-cockpit \
  "${HOME}/.local/bin/maintainer-cockpit"
maintainer-cockpit version
```

Use the `arm64` archive and directory names on an ARM64 host. Verify the
archive against the signed checksums before installation. Run
`maintainer-cockpit help` for commands and flags.

## Update

[Deployment operations](operations.md) covers reloads, forced refreshes,
retention, diagnostics, and optional OpenTelemetry. Start with
[Troubleshooting](troubleshooting.md) when validation, startup, collection, or
analysis fails.

Before an update, back up the SQLite volume and read the release notes. Pull
the new immutable image tag, rerun `config-check` and `setup-check` with that
image, stop and remove the old container, and recreate it with the new tag.
Never delete the `maintainer-cockpit-data` volume during this process.

A structurally incompatible database fails startup. Preserve its backup before
creating a fresh database.

## Run from source

Contributors can run from source with Go 1.26 and Node.js 24:

```sh
git clone https://github.com/ArthurSens/maintainer-cockpit.git
cd maintainer-cockpit
make config-check
make start
```

The default development listener is http://127.0.0.1:8767. See
[Contributing](../CONTRIBUTING.md) for the complete development workflow.
