# syntax=docker/dockerfile:1

# ============================================================================
# code-service image: the HIGH-privilege half of the two-container setup.
#
# It runs the Claude Code CLI headless against the bind-mounted repo (/repo),
# so it needs git, the GitHub CLI, and the CLI itself. The gateway container
# talks to it over the compose network at http://code-service:8080/execute.
# See code-service/server.js for the trust model.
# ============================================================================
FROM node:20-alpine

# git + gh (github-cli lives in the Alpine community repo). ca-certificates for
# HTTPS pushes. bash because gh's setup-git and some tooling expect it.
RUN apk add --no-cache git github-cli ca-certificates bash

# Claude Code CLI (pure-JS npm package, runs fine on node:alpine).
RUN npm install -g @anthropic-ai/claude-code

WORKDIR /app
COPY server.js entrypoint.sh /app/
RUN chmod +x /app/entrypoint.sh

ENV PORT=8080 \
    REPO_DIR=/repo
WORKDIR /repo

# Listens only on the compose network; docker-compose.yml never publishes it.
ENTRYPOINT ["/app/entrypoint.sh"]
