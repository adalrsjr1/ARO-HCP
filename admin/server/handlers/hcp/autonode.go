// Copyright 2026 Microsoft Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hcp

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/corecosmosstorage"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// HCPDesiredAutoNodeHandler sets ServiceProviderClusterSpec
// DesiredAutoNodeEnabled (and, optionally, DesiredAutoNodeKarpenterAzureClientID)
// on the per-cluster ServiceProviderCluster. This is a validation-only
// endpoint for the AutoNode/Karpenter proof-of-concept: it intentionally
// supports only enabling (enabled=true). There is no disable path yet, so an
// absent or false "enabled" value is rejected rather than silently
// persisted, to avoid leaving confusing inert state on the document.
//
// clientId is optional and is not validated or provisioned by this handler —
// it must name the client ID of a managed identity the caller has already
// provisioned out-of-band (with a federated credential trusting this
// cluster's OIDC issuer for the karpenter service account). Azure AutoNode
// requires this identity; without it the hypershift-operator's
// karpenter-operator component will fail to reconcile even though the
// ApplyDesire applies successfully.
//
// See the AutoNodeEnabler backend controller for how this intent is turned
// into a kube-applier ApplyDesire.
type HCPDesiredAutoNodeHandler struct {
	resourcesDBClient corecosmosstorage.ResourcesDBClient
}

func NewHCPDesiredAutoNodeHandler(resourcesDBClient corecosmosstorage.ResourcesDBClient) *HCPDesiredAutoNodeHandler {
	return &HCPDesiredAutoNodeHandler{resourcesDBClient: resourcesDBClient}
}

// desiredAutoNodeRequest is the wire shape for the request body. Enabled is a
// pointer-bool so an omitted field can be distinguished from an explicit
// false; both are rejected today since disabling AutoNode is not supported.
// ClientID is optional; see HCPDesiredAutoNodeHandler for what it must name.
type desiredAutoNodeRequest struct {
	Enabled  *bool   `json:"enabled"`
	ClientID *string `json:"clientId,omitempty"`
}

func (h *HCPDesiredAutoNodeHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) error {
	resourceID, err := utils.ResourceIDFromContext(request.Context())
	if err != nil {
		return coreapi.NewCloudError(http.StatusBadRequest, coreapi.CloudErrorCodeInvalidRequestContent, "", "invalid resource identifier in request")
	}

	var body desiredAutoNodeRequest
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		return coreapi.NewCloudError(http.StatusBadRequest, coreapi.CloudErrorCodeInvalidRequestContent, "", "invalid JSON body: %v", err)
	}
	// Disabling AutoNode is not implemented yet: reject both an omitted
	// "enabled" field and an explicit false, rather than silently persisting
	// state that no controller acts on.
	if body.Enabled == nil || !*body.Enabled {
		return coreapi.NewCloudError(http.StatusBadRequest, coreapi.CloudErrorCodeInvalidRequestContent, "", "enabled must be set to true; disabling AutoNode is not supported")
	}
	if body.ClientID != nil && *body.ClientID == "" {
		return coreapi.NewCloudError(http.StatusBadRequest, coreapi.CloudErrorCodeInvalidRequestContent, "", "clientId must not be empty; omit the field to leave it unset")
	}

	existing, err := corecosmosstorage.GetOrCreateServiceProviderCluster(request.Context(), h.resourcesDBClient, resourceID)
	if err != nil {
		return fmt.Errorf("failed to get ServiceProviderCluster: %w", err)
	}

	replacement := existing.DeepCopy()
	replacement.Spec.DesiredAutoNodeEnabled = body.Enabled
	if body.ClientID != nil {
		replacement.Spec.DesiredAutoNodeKarpenterAzureClientID = body.ClientID
	}

	_, err = h.resourcesDBClient.ServiceProviderClusters(resourceID.SubscriptionID, resourceID.ResourceGroupName, resourceID.Name).Replace(request.Context(), replacement, nil)
	if err != nil {
		return fmt.Errorf("failed to replace ServiceProviderCluster: %w", err)
	}

	_, err = coreapi.WriteJSONResponse(writer, http.StatusOK, desiredAutoNodeRequest{
		Enabled:  body.Enabled,
		ClientID: replacement.Spec.DesiredAutoNodeKarpenterAzureClientID,
	})
	return utils.TrackError(err)
}
