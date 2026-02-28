#!/bin/bash

# migrate-to-ondemand.sh
# This script finds all deployments in the inference namespace with the label
# inference.eh-ops.io/managed=true that have 0 replicas and deletes them
# along with their associated services.
#
# The script is idempotent and safe to run multiple times.

set -euo pipefail

NAMESPACE="inference"
LABEL="inference.eh-ops.io/managed=true"

log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $1"
}

log "Starting migration to on-demand..."
log "Looking for deployments with label ${LABEL} in namespace ${NAMESPACE}"

# Find all deployments with the specified label
DEPLOYMENTS=$(kubectl get deployments -n "${NAMESPACE}" -l "${LABEL}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)

if [ -z "${DEPLOYMENTS}" ]; then
    log "No deployments found with label ${LABEL} in namespace ${NAMESPACE}"
    exit 0
fi

log "Found deployments: ${DEPLOYMENTS}"

# Track deployments with 0 replicas
ZERO_REPLICA_DEPLOYMENTS=()

for DEPLOYMENT in ${DEPLOYMENTS}; do
    # Get the replica count for this deployment
    REPLICAS=$(kubectl get deployment "${DEPLOYMENT}" -n "${NAMESPACE}" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "0")

    log "Deployment ${DEPLOYMENT} has ${REPLICAS} replicas"

    if [ "${REPLICAS}" == "0" ]; then
        ZERO_REPLICA_DEPLOYMENTS+=("${DEPLOYMENT}")
    fi
done

if [ ${#ZERO_REPLICA_DEPLOYMENTS[@]} -eq 0 ]; then
    log "No deployments with 0 replicas found. Nothing to do."
    exit 0
fi

log "Found ${#ZERO_REPLICA_DEPLOYMENTS[@]} deployment(s) with 0 replicas to migrate"

# Process each deployment with 0 replicas
for DEPLOYMENT in "${ZERO_REPLICA_DEPLOYMENTS[@]}"; do
    log "Processing deployment: ${DEPLOYMENT}"

    # Find associated services (services that share the same app label or name)
    # First, try to find services with the same app label as the deployment
    APP_LABEL=$(kubectl get deployment "${DEPLOYMENT}" -n "${NAMESPACE}" -o jsonpath='{.metadata.labels.app}' 2>/dev/null || true)

    if [ -n "${APP_LABEL}" ]; then
        SERVICES=$(kubectl get services -n "${NAMESPACE}" -l "app=${APP_LABEL}" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
    else
        # Fallback: find services with matching name prefix
        SERVICES=$(kubectl get services -n "${NAMESPACE}" -o jsonpath='{.items[?(@.metadata.name=="'"${DEPLOYMENT}"'")].metadata.name}' 2>/dev/null || true)

        # Also check for common naming conventions (deployment-name, deployment-name-svc)
        for SUFFIX in "" "-svc" "-service"; do
            SVC_NAME="${DEPLOYMENT}${SUFFIX}"
            if kubectl get service "${SVC_NAME}" -n "${NAMESPACE}" &>/dev/null; then
                if [ -z "${SERVICES}" ]; then
                    SERVICES="${SVC_NAME}"
                else
                    SERVICES="${SERVICES} ${SVC_NAME}"
                fi
            fi
        done
    fi

    # Delete associated services first
    if [ -n "${SERVICES}" ]; then
        for SERVICE in ${SERVICES}; do
            log "Deleting service: ${SERVICE}"
            if kubectl delete service "${SERVICE}" -n "${NAMESPACE}" --ignore-not-found=true 2>/dev/null; then
                log "Successfully deleted service: ${SERVICE}"
            else
                log "Warning: Failed to delete service: ${SERVICE}"
            fi
        done
    else
        log "No associated services found for deployment: ${DEPLOYMENT}"
    fi

    # Delete the deployment
    log "Deleting deployment: ${DEPLOYMENT}"
    if kubectl delete deployment "${DEPLOYMENT}" -n "${NAMESPACE}" --ignore-not-found=true 2>/dev/null; then
        log "Successfully deleted deployment: ${DEPLOYMENT}"
    else
        log "Warning: Failed to delete deployment: ${DEPLOYMENT}"
    fi
done

log "Migration completed successfully"
log "Summary: Processed ${#ZERO_REPLICA_DEPLOYMENTS[@]} deployment(s) with 0 replicas"
