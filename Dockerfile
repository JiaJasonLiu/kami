# syntax=docker/dockerfile:1

# ============================================================================
# kami-gateway image (the Telegram gateway itself).
#
# This is the LOW-privilege half of the two-container setup: it never edits
# source code and never runs a shell for the model. Its only mutable data is
# the bind-mounted state/, workspace/ and agents/ dirs. Coding/self-PR work is
# relayed over the compose network to the separate `code-service` container
# (see docker-compose.yml + code-service/). Keeping the two apart preserves the
# sandbox model in CLAUDE.md: this binary has no os/exec and cannot reach the
# repo source.
# ============================================================================

# ---- build stage -----------------------------------------------------------
# Static, CGO-free build so the runtime image stays minimal. Mirrors the
# `make dist` recipe. The project is standard-library only, so there is no
# `go mod download` step.
FROM golang:1.21-alpine AS build

WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/kami-gateway .

# ---- runtime stage ---------------------------------------------------------
# Alpine (not scratch) so there is a shell for `docker compose run ... setup`
# and for debugging. ca-certificates is required for outbound HTTPS to the AI
# provider, Telegram, and web_fetch / web_search.
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 1000 -h /data kami

COPY --from=build /out/kami-gateway /usr/local/bin/kami-gateway

# $KAMI_HOME. state/, workspace/ and agents/ live here and are bind-mounted
# from the host (see docker-compose.yml) so all mutable data — config, keys,
# each agent's SOUL.md/tools.json/history and its workspace — persists on the
# host and survives image rebuilds. Nothing mutable is baked into the image.
ENV KAMI_HOME=/data
WORKDIR /data
USER kami

# The gateway makes only OUTBOUND connections (long-polls Telegram, calls the
# AI provider, and POSTs coding prompts to the code-service container). It
# listens on no ports, so nothing is EXPOSEd.
ENTRYPOINT ["kami-gateway"]
