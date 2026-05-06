# CRM container image — Fase 0 PR9 (SIN-62215).
#
# Multi-stage build: a Go builder produces a statically linked binary, then we
# copy it into a distroless runtime that has no shell and no package manager.
# Image is pushed to ghcr.io/pericles-luz/crm:<git-sha> by .github/workflows/cd-stg.yml
# and consumed by deploy/compose/compose.stg.yml via APP_IMAGE=...@sha256:<digest>.
#
# Pin policy (CEO follow-up SIN-62208 / AC #6 of SIN-62215): builder and runtime
# are pinned by SHA256 digest. Bumping the digests is documented in
# docs/deploy/staging.md ("Bumping infra image digests"). Tag is kept alongside
# the digest so a human reading the file sees the version, but Docker resolves
# strictly by digest.

# syntax=docker/dockerfile:1.7

# --- builder stage ---------------------------------------------------------
# DO NOT MERGE — SIN-62301 positive-control throwaway. Regressing the builder
# to golang:1.24.5-alpine to confirm the docker-smoke PR-time gate fails
# fail-closed when go.mod's `toolchain go1.25.9` directive is not satisfied
# by the build sandbox. See https://github.com/pericles-luz/crm/pull/45.
FROM golang:1.24.5-alpine AS builder

WORKDIR /src

# Module cache layer — only changes when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

# Source layer.
COPY cmd ./cmd
COPY internal ./internal
COPY adapters ./adapters

# CGO_ENABLED=0 + -trimpath + -ldflags="-s -w" yields a small, reproducible,
# statically linked binary. GOFLAGS prevents the toolchain from auto-downloading
# a different Go version at build time (we want the pinned 1.25.9 alpine image,
# matching go.mod's `toolchain go1.25.9` directive — bumped here as a follow-up
# to SIN-62297 c4b2c73 toolchain pin so the in-container compile matches CI).
ENV CGO_ENABLED=0 GOFLAGS=-mod=readonly GOTOOLCHAIN=local
RUN go build -trimpath -ldflags="-s -w" -o /out/crm ./cmd/server

# --- runtime stage ---------------------------------------------------------
# distroless/static-debian12:nonroot — no shell, no apt, UID/GID 65532, only
# CA certs and the binary. See gcr.io/distroless/static for the contract.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:a9329520abc449e3b14d5bc3a6ffae065bdde0f02667fa10880c49b35c109fd1

COPY --from=builder /out/crm /app/crm

USER 65532:65532
WORKDIR /app
EXPOSE 8080
ENV HTTP_ADDR=":8080"

ENTRYPOINT ["/app/crm"]
