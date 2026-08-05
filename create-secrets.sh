#!/bin/bash

# Creates the registry pull secret. The application's own secrets are NOT
# created here — they live SOPS-encrypted in k8s/secrets.enc.yaml and are
# decrypted in-cluster by Flux.
#
# Reads .env.secrets (not version-controlled). Required keys:
#   GHCR_USERNAME  — GitHub username for container registry
#   GHCR_PAT       — GitHub personal access token (read:packages scope)
#   GHCR_EMAIL     — GitHub account email

set -euo pipefail

NAMESPACE="${NAMESPACE:-instalker}"

set -o allexport
source .env.secrets
set +o allexport

kubectl create namespace "${NAMESPACE}" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl create secret docker-registry ghcr-secret \
  --namespace="${NAMESPACE}" \
  --docker-server=ghcr.io \
  --docker-username="${GHCR_USERNAME}" \
  --docker-password="${GHCR_PAT}" \
  --docker-email="${GHCR_EMAIL}" \
  --dry-run=client -o yaml | kubectl apply -f -
