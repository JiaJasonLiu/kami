#!/bin/sh
# Prepare git/gh so the Claude Code CLI can commit, push branches and open PRs
# against the bind-mounted /repo, then hand off to the HTTP server.
set -e

REPO_DIR="${REPO_DIR:-/repo}"

# The repo is bind-mounted and owned by the host user, not by this container's
# user, so git would refuse to operate on it without this.
git config --global --add safe.directory "$REPO_DIR" 2>/dev/null || true

# Commit identity (override via env in docker-compose / .env).
git config --global user.name  "${GIT_AUTHOR_NAME:-kami-bot}" 2>/dev/null || true
git config --global user.email "${GIT_AUTHOR_EMAIL:-kami-bot@users.noreply.github.com}" 2>/dev/null || true

# Wire gh + git to the token from .env so `git push` and `gh pr create` work.
# GH_TOKEN is picked up by gh automatically; setup-git makes plain git https
# pushes use it too. No token -> the service still runs, but pushes will fail
# with a clear auth error rather than at container start.
if [ -n "$GH_TOKEN" ] || [ -n "$GITHUB_TOKEN" ]; then
  gh auth setup-git 2>/dev/null || true
fi

exec node /app/server.js
