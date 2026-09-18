FROM golang:1.26-alpine@sha256:ce864e7223ac17b1775e6fd0b4c0db580c2eb50e7953a427916379e4b92a1628 AS build

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
    -o /out/maintainer-cockpit \
    ./cmd/maintainer-cockpit
RUN mkdir -p /out/data /out/config /out/fixtures \
    && cp config/maintainer-cockpit.yaml /out/config/config.yaml \
    && cp testdata/container-smoke-config.yaml /out/config/container-smoke.yaml \
    && cp testdata/demo-pull-requests.json /out/fixtures/demo-pull-requests.json

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=65532:65532 /out/maintainer-cockpit /maintainer-cockpit
COPY --from=build --chown=65532:65532 /out/data /var/lib/maintainer-cockpit
COPY --from=build --chown=65532:65532 /out/config /etc/maintainer-cockpit
COPY --from=build --chown=65532:65532 /out/fixtures /usr/share/maintainer-cockpit/fixtures

USER 65532:65532
EXPOSE 8765
VOLUME ["/var/lib/maintainer-cockpit"]

ENTRYPOINT ["/maintainer-cockpit"]
CMD ["serve", "--config", "/etc/maintainer-cockpit/config.yaml", "--database", "/var/lib/maintainer-cockpit/maintainer-cockpit.db", "--listen", "0.0.0.0:8765"]
