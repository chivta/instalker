#!/bin/bash

# All secrets are read from .env.secrets (not version-controlled).
# Required keys:
#   GHCR_USERNAME  — GitHub username for container registry
#   GHCR_PAT       — GitHub personal access token (read:packages scope)
#   GHCR_EMAIL     — GitHub account email
#   USERNAME       — Instagram account used to watch the targets
#   PASSWORD       — Instagram password (fallback only; see IG_SESSIONID)
#   IG_SESSIONID   — Instagram `sessionid` cookie from a logged-in browser
#   BOT_TOKEN      — Telegram bot token
#   CHAT_ID        — Telegram chat that receives the notifications
#   TARGETS        — optional comma-separated usernames; empty = watch who USERNAME follows

set -euo pipefail

NAMESPACE="${NAMESPACE:-instalker}"

set -o allexport
source .env.secrets
set +o allexport

kubectl create namespace "${NAMESPACE}" \
  --dry-run=client -o yaml | kubectl apply -f -

# Registry auth
kubectl create secret docker-registry ghcr-secret \
  --namespace="${NAMESPACE}" \
  --docker-server=ghcr.io \
  --docker-username="${GHCR_USERNAME}" \
  --docker-password="${GHCR_PAT}" \
  --docker-email="${GHCR_EMAIL}" \
  --dry-run=client -o yaml | kubectl apply -f -

# App secrets
kubectl create secret generic app-secrets \
  --namespace="${NAMESPACE}" \
  --from-literal=USERNAME="${USERNAME}" \
  --from-literal=PASSWORD="${PASSWORD}" \
  --from-literal=IG_SESSIONID="${IG_SESSIONID}" \
  --from-literal=BOT_TOKEN="${BOT_TOKEN}" \
  --from-literal=CHAT_ID="${CHAT_ID}" \
  --from-literal=TARGETS="${TARGETS:-}" \
  --dry-run=client -o yaml | kubectl apply -f -
