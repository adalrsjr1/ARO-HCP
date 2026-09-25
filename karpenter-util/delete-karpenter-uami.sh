#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat <<'EOF'
Usage: delete-karpenter-uami.sh <state-file>

Deletes the Karpenter identity recorded by create-karpenter-uami.sh. The
resource group is also deleted, unless it was reused from a pre-existing
resource group, in which case it is left in place.
EOF
}

if [[ $# -ne 1 ]]; then
    usage >&2
    exit 2
fi

for command_name in az jq; do
    if ! command -v "${command_name}" >/dev/null 2>&1; then
        echo "Required command not found: ${command_name}" >&2
        exit 1
    fi
done

STATE_FILE="$1"
if [[ ! -f "${STATE_FILE}" ]]; then
    echo "State file not found: ${STATE_FILE}" >&2
    exit 1
fi

if ! jq -e '
    (.subscriptionId | type == "string" and length > 0) and
    (.resourceGroup | type == "string" and length > 0) and
    (.identityResourceId | type == "string")
' "${STATE_FILE}" >/dev/null; then
    echo "State file is invalid: ${STATE_FILE}" >&2
    exit 1
fi

SUBSCRIPTION_ID="$(jq -r '.subscriptionId' "${STATE_FILE}")"
RESOURCEGROUP="$(jq -r '.resourceGroup' "${STATE_FILE}")"
DP_KARPENTER_UAMI="$(jq -r '.identityResourceId' "${STATE_FILE}")"
RESOURCEGROUP_CREATED="$(jq -r '.resourceGroupCreated // true' "${STATE_FILE}")"
ACTIVE_SUBSCRIPTION_ID="$(az account show --query id --output tsv)"

if [[ "${ACTIVE_SUBSCRIPTION_ID}" != "${SUBSCRIPTION_ID}" ]]; then
    echo "Active Azure subscription does not match the recorded subscription." >&2
    echo "Active:   ${ACTIVE_SUBSCRIPTION_ID}" >&2
    echo "Recorded: ${SUBSCRIPTION_ID}" >&2
    echo "Run: az account set --subscription '${SUBSCRIPTION_ID}'" >&2
    exit 1
fi

if [[ -n "${DP_KARPENTER_UAMI}" ]]; then
    if az identity show --ids "${DP_KARPENTER_UAMI}" >/dev/null 2>&1; then
        echo "Deleting Karpenter identity ${DP_KARPENTER_UAMI}..."
        az identity delete --ids "${DP_KARPENTER_UAMI}"
    else
        echo "Karpenter identity is already absent."
    fi
fi

if [[ "${RESOURCEGROUP_CREATED}" != "true" ]]; then
    echo "Resource group ${RESOURCEGROUP} was reused, not created; leaving it in place."
elif [[ "$(az group exists --name "${RESOURCEGROUP}")" == "true" ]]; then
    echo "Deleting dedicated resource group ${RESOURCEGROUP} and its remaining contents..."
    az group delete --name "${RESOURCEGROUP}" --yes
else
    echo "Resource group ${RESOURCEGROUP} is already absent."
fi

rm "${STATE_FILE}"
echo "Compensation completed; removed state file ${STATE_FILE}."