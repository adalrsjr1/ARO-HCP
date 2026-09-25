# Karpenter AutoNode POC utilities

## Scope

- Provision Azure prerequisites for an ARO HCP 4.22 cluster with a dedicated Karpenter identity.
- Generate a direct Cluster Service payload.
- Stop at the current Cluster Service validation blocker.
- The scripts do not submit the generated payload.
- All customer resources are placed in one disposable resource group.

## Prerequisites

- Run commands from the repository root.
- Install and authenticate these tools:
  - `az`
  - `jq`
  - `openssl`
  - `ocm`
- Select the intended Azure subscription:

  ```bash
  az account show --query '{subscription:id,tenant:tenantId}' --output yaml
  ```

- Use a new, disposable customer resource group. The create script refuses to use an existing group.
- Use a separate managed resource group name.
- Use a concrete OpenShift 4.22 release ID, such as `openshift-v4.22.11`.

## 1. Create the resource group and Karpenter identity

```bash
./karpenter-util/create-karpenter-uami.sh \
  asam-karpenter \
  asam-karpenter-rg \
  westus3
```

- Record the state file printed by the script.
- The state file is created under:

  ```text
  ${XDG_STATE_HOME:-$HOME/.local/state}/aro-hcp-karpenter-uami/
  ```

- Export it for the remaining steps:

  ```bash
  export STATE_FILE=/path/printed/by/the/script.json
  ```

## 2. Provision the network

```bash
./karpenter-util/provision-karpenter-network.sh "$STATE_FILE"
```

Default resources:

- VNet: `<cluster-name>-vnet`
- Subnet: `<cluster-name>-subnet`
- NSG: `<cluster-name>-nsg`
- VNet CIDR: `10.0.0.0/16`
- Subnet CIDR: `10.0.0.0/24`

The script records the subnet and NSG resource IDs in the state file.

## 3. Provision cluster identities

```bash
./karpenter-util/provision-cluster-identities.sh "$STATE_FILE"
```

The script creates and records:

- 8 standard control-plane operator identities.
- 3 standard data-plane operator identities.
- 1 service managed identity.
- The previously created Karpenter data-plane identity.

Inspect the result:

```bash
jq '.identities' "$STATE_FILE"
```

## 4. Provision customer-managed KMS

```bash
./karpenter-util/provision-cluster-kms.sh "$STATE_FILE"
```

The script creates or reuses:

- A KMS control-plane identity.
- An RBAC-enabled Key Vault.
- An RSA 2048 etcd encryption key.

The Key Vault and key are deployed from `karpenter-util/cluster-kms.bicep`.

Inspect the result:

```bash
jq '{kms, kmsIdentity: .identities.controlPlane.kms}' "$STATE_FILE"
```

Personal-environment requirements:

- The caller must be allowed to deploy the Key Vault and key.
- The identity used by Cluster Service for the personal environment must have Key Vault Crypto User access.

## 5. Generate the Cluster Service payload

```bash
./karpenter-util/generate-cluster-service-payload.sh \
  "$STATE_FILE" \
  openshift-v4.22.11 \
  cluster-test-kms-v2.json \
  asam-karpenter-managed-rg
```

The generator checks that:

- The release is a concrete OpenShift 4.22 release.
- Customer and managed resource groups differ.
- Network, KMS, and all required identities are present.
- Karpenter is present as a data-plane identity.
- The obsolete `multi_az` field is absent.
- An existing output file is not overwritten.

Review the payload:

```bash
jq . cluster-test-kms-v2.json
```

## 6. Connect to Cluster Service

- Port-forward the service from the personal service cluster:

  ```bash
  kubectl port-forward \
    --namespace clusters-service \
    service/clusters-service \
    8000:8000
  ```

- Login with OCM:

  ```bash
  ocm login --url=http://localhost:8000 --use-auth-code
  ```

## 7. Reproduce the current blocker

Submit only after reviewing the payload:

```bash
ocm post /api/aro_hcp/v1alpha1/clusters < cluster-test-kms-v2.json
```

Expected result with the current Cluster Service implementation:

```text
Extra managed identities without corresponding feature enablement for 4.22.11 openshift version: [karpenter].
```

Reason:

- Cluster Service recognizes Karpenter as an optional 4.22 data-plane identity.
- Azure AutoNode is not implemented in Cluster Service.
- No request field activates the optional Karpenter identity.
- HyperShift feature flags enable operator support in the HyperShift binary, but do not activate the Cluster Service identity requirement.
- The request fails synchronously with HTTP 400. No cluster is created.

Do not remove Karpenter from the payload and treat that as a successful AutoNode test. A cluster created without the identity cannot authenticate the Karpenter operand to Azure.

## Cleanup

The cleanup script deletes the entire customer resource group recorded in the state file, including its network, identities, Key Vault, key, and deployments.

```bash
./karpenter-util/delete-karpenter-uami.sh "$STATE_FILE"
```

- Confirm that the state file points to the disposable resource group before running cleanup.
- The managed resource group is not created when the request fails with HTTP 400.
