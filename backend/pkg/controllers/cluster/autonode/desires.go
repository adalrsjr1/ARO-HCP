// Copyright 2026 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package autonode

import (
	"encoding/json"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	hsv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"

	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/api/kubeapplierapi"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// autoNodeApplyDesireName is the well-known, single ApplyDesire name this
// controller writes per cluster. There is only ever one: this is a
// validation-only POC that sets a single field (spec.autoNode) on the
// existing HostedCluster, not a family of desires like the backup schedules.
const autoNodeApplyDesireName = "autonode-karpenter-poc"

// autoNodeFieldManager is a dedicated, non-default field manager for this
// POC's server-side-apply writes. Using a distinct manager (rather than
// kube-applier's default) keeps ownership of spec.autoNode clearly
// attributable to this validation path, separate from any legacy writer
// that may still be reconciling the rest of the HostedCluster object, and
// makes manual field removal during retirement unambiguous (see the
// operator runbook: `kubectl patch ... --field-manager=aro-hcp-autonode-validation`
// style removal, not ad hoc edits).
const autoNodeFieldManager = "aro-hcp-autonode-validation"

// partialHostedCluster is the minimal HostedCluster shape this controller
// server-side-applies. It intentionally carries only apiVersion/kind,
// identifying metadata, and spec.autoNode — never a full HostedCluster (or
// even a full HostedClusterSpec), so this field manager only ever claims
// ownership of the one field it actually cares about and never fights any
// other writer over the rest of the object.
type partialHostedCluster struct {
	metav1.TypeMeta `json:",inline"`
	Metadata        partialObjectMeta        `json:"metadata"`
	Spec            partialHostedClusterSpec `json:"spec"`
}

type partialObjectMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type partialHostedClusterSpec struct {
	AutoNode autoNodeSpec `json:"autoNode"`
}

// autoNodeSpec, provisionerConfigSpec, karpenterConfigSpec, and
// karpenterAzureConfigSpec are local mirrors of the equivalent types in the
// custom hypershift-operator fork under validation
// (/home/asampaio/Coding/tmp-karpenter/hypershift, api/hypershift/v1beta1/hostedcluster_types.go:
// AutoNode, ProvisionerConfig, KarpenterConfig, KarpenterAzureConfig). They
// are defined locally, rather than imported from hsv1beta1, because the
// pinned github.com/openshift/hypershift/api dependency in backend/go.mod is
// stock upstream hypershift, whose KarpenterConfig only supports AWS — it
// has no Azure field or AzureClientID type. Pointing go.mod at the custom
// fork (via a replace directive) would be exactly the kind of production
// wiring change this validation effort is explicitly avoiding, so we instead
// hand-marshal JSON matching the fork's wire schema. Field names/json tags
// must be kept in sync with the fork if its AutoNode schema changes.
type autoNodeSpec struct {
	Provisioner provisionerConfigSpec `json:"provisionerConfig"`
}

type provisionerConfigSpec struct {
	Name      string              `json:"name"`
	Karpenter karpenterConfigSpec `json:"karpenter"`
}

type karpenterConfigSpec struct {
	Platform string                   `json:"platform"`
	Azure    karpenterAzureConfigSpec `json:"azure"`
}

type karpenterAzureConfigSpec struct {
	ClientID string `json:"clientID"`
}

// buildAutoNodePayload builds the partial-HostedCluster JSON that turns on
// Azure Karpenter AutoNode. clientID must be non-empty: the fork's
// KarpenterAzureConfig.ClientID is a required field once
// KarpenterConfig.Platform is Azure (the karpenter-operator hard-errors
// without it), so callers must gate on a non-empty clientID before calling
// this — see autoNodeSyncer.SyncOnce.
func buildAutoNodePayload(target kubeapplierapi.ResourceReference, clientID string) ([]byte, error) {
	if clientID == "" {
		return nil, utils.TrackError(fmt.Errorf("clientID must not be empty"))
	}
	obj := partialHostedCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: hsv1beta1.SchemeGroupVersion.String(),
			Kind:       "HostedCluster",
		},
		Metadata: partialObjectMeta{
			Name:      target.Name,
			Namespace: target.Namespace,
		},
		Spec: partialHostedClusterSpec{
			AutoNode: autoNodeSpec{
				Provisioner: provisionerConfigSpec{
					// "Karpenter": matches hsv1beta1.ProvisionerKarpenter's
					// value in both stock upstream and the fork.
					Name: "Karpenter",
					Karpenter: karpenterConfigSpec{
						Platform: "Azure",
						Azure: karpenterAzureConfigSpec{
							ClientID: clientID,
						},
					},
				},
			},
		},
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, utils.TrackError(fmt.Errorf("failed to marshal partial HostedCluster AutoNode payload: %w", err))
	}
	return raw, nil
}

// buildAutoNodeApplyDesire builds the single cluster-scoped ApplyDesire that
// server-side-applies spec.autoNode onto the cluster's existing HostedCluster.
func buildAutoNodeApplyDesire(
	subscriptionID, resourceGroupName, clusterName string,
	managementClusterResourceID *azcorearm.ResourceID,
	target kubeapplierapi.ResourceReference,
	clientID string,
) (*kubeapplierapi.ApplyDesire, error) {
	resourceIDStr := kubeapplierapi.ToClusterScopedApplyDesireResourceIDString(
		subscriptionID, resourceGroupName, clusterName, autoNodeApplyDesireName,
	)
	resourceID, err := azcorearm.ParseResourceID(resourceIDStr)
	if err != nil {
		return nil, utils.TrackError(fmt.Errorf("failed to parse ApplyDesire resource ID: %w", err))
	}

	raw, err := buildAutoNodePayload(target, clientID)
	if err != nil {
		return nil, err
	}

	return &kubeapplierapi.ApplyDesire{
		CosmosMetadata: coreapi.CosmosMetadata{
			ResourceID:   resourceID,
			PartitionKey: strings.ToLower(managementClusterResourceID.String()),
		},
		Spec: kubeapplierapi.ApplyDesireSpec{
			ManagementCluster: managementClusterResourceID,
			Type:              kubeapplierapi.ApplyDesireTypeServerSideApply,
			TargetItem:        target,
			ServerSideApply: &kubeapplierapi.ServerSideApplyConfig{
				FieldManager: ptr.To(autoNodeFieldManager),
				KubeContent:  &runtime.RawExtension{Raw: raw},
			},
		},
		Tags: map[string]string{
			kubeapplierapi.TagControllerName: AutoNodeEnablerControllerName,
		},
	}, nil
}
