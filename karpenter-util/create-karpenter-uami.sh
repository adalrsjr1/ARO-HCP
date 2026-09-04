#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat <<'EOF'
Usage: create-karpenter-uami.sh <cluster-name> <oidc-issuer-url> <vnet-resource-group> <vnet-name> <node-resource-group> [identity-resource-group] [location]

Creates everything AutoNode/Karpenter needs to authenticate to Azure and
manage nodes for a HostedCluster:
  1. A user-assigned managed identity (UAMI).
  2. A federated identity credential (FIC) on that UAMI, trusting OIDC tokens
     minted by this HostedCluster's own issuer for the karpenter service
     account (this is what lets the karpenter pod exchange its projected
     token for an Azure AD access token, instead of a client secret).
  3. RBAC role assignments granting the UAMI the Azure permissions Karpenter
     needs at runtime: reading the customer VNet, and managing VMs/NICs/disks
     in the node (managed) resource group.

Required inputs (all cluster-specific, so they can't be defaulted):
  oidc-issuer-url      HostedCluster's OIDC issuer URL
                        (oc get hostedcluster <name> -n <ns> -o jsonpath='{.spec.issuerURL}')
  vnet-resource-group  Resource group containing the customer VNet
  vnet-name            Name of the customer VNet
  node-resource-group  AKS-style "managed"/node resource group where Karpenter
                        creates VMs (HostedCluster.Spec.Platform.Azure.ResourceGroupName)

If the identity resource group does not exist, a dedicated one is created for
it. If it already exists (e.g. the one used by a cluster in the e2e personal
dev environment), it is reused and left in place on cleanup.
The identity resource group defaults to <cluster-name>-rg and the location to
westus3.
EOF
}

if [[ $# -lt 5 || $# -gt 7 ]]; then
    usage >&2
    exit 2
fi

for command_name in az jq openssl; do
    if ! command -v "${command_name}" >/dev/null 2>&1; then
        echo "Required command not found: ${command_name}" >&2
        exit 1
    fi
done

CS_CLUSTER_NAME="$1"
OIDC_ISSUER_URL="$2"
VNET_RESOURCE_GROUP="$3"
VNET_NAME="$4"
NODE_RESOURCE_GROUP="$5"
RESOURCEGROUP="${6:-${CS_CLUSTER_NAME}-rg}"
LOCATION="${7:-westus3}"

# Fail fast with a clear message instead of building an invalid Azure scope
# (e.g. ".../resourceGroups//providers/...") if a caller's jsonpath extraction
# produced an empty string for one of these.
for required_var in CS_CLUSTER_NAME OIDC_ISSUER_URL VNET_RESOURCE_GROUP VNET_NAME NODE_RESOURCE_GROUP; do
    if [[ -z "${!required_var}" ]]; then
        echo "Required argument ${required_var} is empty; check how it was derived." >&2
        exit 2
    fi
done
OPERATORS_UAMIS_SUFFIX="$(openssl rand -hex 3)"
IDENTITY_NAME="${CS_CLUSTER_NAME}-dp-karpenter-${OPERATORS_UAMIS_SUFFIX}"
SUBSCRIPTION_ID="$(az account show --query id --output tsv)"
STATE_DIR="${XDG_STATE_HOME:-${HOME}/.local/state}/aro-hcp-karpenter-uami"
STATE_FILE="${STATE_DIR}/${CS_CLUSTER_NAME}-${OPERATORS_UAMIS_SUFFIX}.json"

# The karpenter operand's token-minter init container is hardcoded (both in
# the hypershift fork's control-plane-operator/.../v2/karpenter/deployment.go
# and in karpenter-operator's hcp_controller.go tokenMinterContainer()) to
# request tokens with audience "openshift" for the "kube-system:karpenter"
# service account. The FIC's audience/subject must match those exactly, or
# Azure AD rejects the token exchange with AADSTS700212 ("No matching
# federated identity record found for presented assertion audience").
readonly KARPENTER_FIC_AUDIENCE="openshift"
readonly KARPENTER_FIC_SUBJECT="system:serviceaccount:kube-system:karpenter"
readonly FIC_NAME="karpenter-fic"

RESOURCEGROUP_CREATED="false"
if [[ "$(az group exists --name "${RESOURCEGROUP}")" == "true" ]]; then
    echo "Reusing existing resource group: ${RESOURCEGROUP}"
    echo "It will not be deleted by cleanup; only the identity created here will be." >&2
else
    mkdir -p "${STATE_DIR}"
    chmod 700 "${STATE_DIR}"

    echo "Creating dedicated resource group ${RESOURCEGROUP} in ${LOCATION}..."
    az group create \
        --name "${RESOURCEGROUP}" \
        --location "${LOCATION}" \
        --output none
    RESOURCEGROUP_CREATED="true"
fi

mkdir -p "${STATE_DIR}"
chmod 700 "${STATE_DIR}"

jq -n \
    --arg subscriptionId "${SUBSCRIPTION_ID}" \
    --arg clusterName "${CS_CLUSTER_NAME}" \
    --arg resourceGroup "${RESOURCEGROUP}" \
    --arg location "${LOCATION}" \
    --arg suffix "${OPERATORS_UAMIS_SUFFIX}" \
    --argjson resourceGroupCreated "${RESOURCEGROUP_CREATED}" \
    --arg oidcIssuerUrl "${OIDC_ISSUER_URL}" \
    --arg vnetResourceGroup "${VNET_RESOURCE_GROUP}" \
    --arg vnetName "${VNET_NAME}" \
    --arg nodeResourceGroup "${NODE_RESOURCE_GROUP}" \
    --arg ficName "${FIC_NAME}" \
    '{
        subscriptionId: $subscriptionId,
        clusterName: $clusterName,
        resourceGroup: $resourceGroup,
        resourceGroupCreated: $resourceGroupCreated,
        location: $location,
        suffix: $suffix,
        identityResourceId: "",
        oidcIssuerUrl: $oidcIssuerUrl,
        vnetResourceGroup: $vnetResourceGroup,
        vnetName: $vnetName,
        nodeResourceGroup: $nodeResourceGroup,
        federatedCredentialName: $ficName
    }' >"${STATE_FILE}"
chmod 600 "${STATE_FILE}"

cleanup_hint() {
    echo >&2
    echo "Creation did not complete. Compensate with:" >&2
    echo "  delete-karpenter-uami.sh '${STATE_FILE}'" >&2
    echo "(deleting the identity also removes its federated credential and role assignments automatically)" >&2
}
trap cleanup_hint ERR

echo "Creating Karpenter user-assigned managed identity ${IDENTITY_NAME}..."
DP_KARPENTER_UAMI="$(
    az identity create \
        --name "${IDENTITY_NAME}" \
        --resource-group "${RESOURCEGROUP}" \
        --location "${LOCATION}" \
        --query id \
        --output tsv
)"

temporary_state="${STATE_FILE}.tmp"
jq --arg identityResourceId "${DP_KARPENTER_UAMI}" \
    '.identityResourceId = $identityResourceId' \
    "${STATE_FILE}" >"${temporary_state}"
mv "${temporary_state}" "${STATE_FILE}"
chmod 600 "${STATE_FILE}"

DP_KARPENTER_CLIENT_ID="$(az identity show --ids "${DP_KARPENTER_UAMI}" --query clientId --output tsv)"

# On a freshly created identity, its service principal can take anywhere from a
# few seconds up to ~1-2 minutes to replicate through AAD graph. Role
# assignment creation resolves --assignee against that graph, so on a new
# identity it commonly fails once or twice with "Cannot find user or service
# principal in graph database for '<clientId>'" before it becomes visible.
# Retry with backoff instead of failing outright.
role_assignment_create_with_retry() {
    sleep 5 # buffer time to mitigate race
    local max_attempts=10
    local attempt=1
    local delay=6
    while true; do
        if az role assignment create "$@" --output none 2>/tmp/role-assignment-error.$$; then
            rm -f /tmp/role-assignment-error.$$
            return 0
        fi
        if ! grep -q "Cannot find user or service principal in graph database" /tmp/role-assignment-error.$$; then
            cat /tmp/role-assignment-error.$$ >&2
            rm -f /tmp/role-assignment-error.$$
            return 1
        fi
        if [[ "${attempt}" -ge "${max_attempts}" ]]; then
            echo "Service principal for ${DP_KARPENTER_CLIENT_ID} still not visible in AAD graph after ${max_attempts} attempts." >&2
            cat /tmp/role-assignment-error.$$ >&2
            rm -f /tmp/role-assignment-error.$$
            return 1
        fi
        echo "Service principal not yet replicated to AAD graph, retrying in ${delay}s (attempt ${attempt}/${max_attempts})..." >&2
        sleep "${delay}"
        attempt=$((attempt + 1))
    done
}

# Without this, karpenter's cmd/controller/main.go panics at startup trying to
# exchange its projected service-account token for an Azure AD token
# (WorkloadIdentityCredential authentication failed / AADSTS700212), since no
# federated credential exists yet trusting this cluster's OIDC issuer.
echo "Creating federated identity credential ${FIC_NAME} on ${IDENTITY_NAME}..."
az identity federated-credential create \
    --name "${FIC_NAME}" \
    --identity-name "${IDENTITY_NAME}" \
    --resource-group "${RESOURCEGROUP}" \
    --issuer "${OIDC_ISSUER_URL}" \
    --subject "${KARPENTER_FIC_SUBJECT}" \
    --audiences "${KARPENTER_FIC_AUDIENCE}" \
    --output none

# Karpenter's Azure cloud provider reads the VNet at startup to resolve the
# VNet GUID/subnet used for new VMs' NICs (pkg/cloudprovider/azure calls GET
# .../virtualNetworks/{name}), and needs Microsoft.Network/virtualNetworks/
# subnets/join/action to attach each new VM's NIC to the subnet when creating
# it in the node resource group (an Azure "linked authorization" check: the
# NIC-create permission on the node RG is not enough on its own). Reader is
# not sufficient for the join action, so this needs Network Contributor (or
# a narrower custom role granting at least the join action) on the VNet.
echo "Granting Network Contributor on VNet ${VNET_NAME} (resource group ${VNET_RESOURCE_GROUP})..."
role_assignment_create_with_retry \
    --assignee "${DP_KARPENTER_CLIENT_ID}" \
    --role "Network Contributor" \
    --scope "/subscriptions/${SUBSCRIPTION_ID}/resourceGroups/${VNET_RESOURCE_GROUP}/providers/Microsoft.Network/virtualNetworks/${VNET_NAME}"

# Karpenter creates and deletes VMs, NICs, and disks directly in the node
# (managed) resource group as it scales nodes up/down in response to
# NodeClaims; Contributor is the narrowest built-in role that covers all of
# those operations.
echo "Granting Contributor on node resource group ${NODE_RESOURCE_GROUP}..."
role_assignment_create_with_retry \
    --assignee "${DP_KARPENTER_CLIENT_ID}" \
    --role "Contributor" \
    --scope "/subscriptions/${SUBSCRIPTION_ID}/resourceGroups/${NODE_RESOURCE_GROUP}"

trap - ERR

echo
echo "Created resources:"
az identity show \
    --ids "${DP_KARPENTER_UAMI}" \
    --query '{name:name, resourceGroup:resourceGroup, clientId:clientId, principalId:principalId, id:id}' \
    --output yaml

cat <<EOF

Use these values in the current shell:
  export CS_CLUSTER_NAME='${CS_CLUSTER_NAME}'
  export RESOURCEGROUP='${RESOURCEGROUP}'
  export OPERATORS_UAMIS_SUFFIX='${OPERATORS_UAMIS_SUFFIX}'
  export DP_KARPENTER_UAMI='${DP_KARPENTER_UAMI}'
  export DP_KARPENTER_CLIENT_ID='${DP_KARPENTER_CLIENT_ID}'

State file:
  ${STATE_FILE}

Note: Azure AD/RBAC role assignments can take up to a couple of minutes to
propagate. If karpenter reports a 403 immediately after this script
completes, wait ~60-90s and restart its pod.

Compensate this creation with:
  delete-karpenter-uami.sh '${STATE_FILE}'
EOF
