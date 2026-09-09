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

// Package autonode implements a validation-only backend controller that
// turns ServiceProviderClusterSpec.DesiredAutoNodeEnabled (set via the admin
// API) into a kube-applier ApplyDesire that server-side-applies
// spec.autoNode onto the cluster's existing HostedCluster object. This
// triggers the hypershift-operator to deploy the karpenter-operator, letting
// us validate the custom hypershift-operator's Karpenter support end-to-end
// without any of the production wiring (ARM API field, Cluster Service,
// identity provisioning) that a real AutoNode feature would need.
//
// There is intentionally no coded disable path: this is a POC, not a
// feature, and a manual retirement runbook covers cleanup. See the plan
// document referenced from the originating design discussion for the full
// rationale and the runbook steps.
package autonode

import (
	"context"
	"fmt"
	"time"

	"github.com/Azure/ARO-HCP/backend/pkg/kubeapplierhelpers"
	"github.com/Azure/ARO-HCP/backend/pkg/utils/controllerutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/corecosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/cosmosstorageutils"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/kubeappliercosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/informers/coreinformers"
	"github.com/Azure/ARO-HCP/internal/database/listers/corelisters"
	"github.com/Azure/ARO-HCP/internal/database/listers/kubeapplierlisters"
	unionkubeapplierinformers "github.com/Azure/ARO-HCP/internal/database/unioninformers/kubeapplier"
	"github.com/Azure/ARO-HCP/internal/utils"
)

// AutoNodeEnablerControllerName is the controller name, recorded on the
// ApplyDesire this controller authors via kubeapplierapi.TagControllerName.
const AutoNodeEnablerControllerName = "AutoNodeEnabler"

// autoNodeSyncer reconciles the single AutoNode ApplyDesire for a cluster.
type autoNodeSyncer struct {
	clusterLister                       corelisters.ClusterLister
	serviceProviderClusterLister        corelisters.ServiceProviderClusterLister
	readDesireLister                    kubeapplierlisters.ReadDesireLister
	applyDesireLister                   kubeapplierlisters.ApplyDesireLister
	kubeApplierDBClients                kubeappliercosmosstorage.KubeApplierDBClients
	hostedClusterNamespaceEnvIdentifier string
}

var _ controllerutils.ClusterSyncer = (*autoNodeSyncer)(nil)

// NewAutoNodeEnablerController wires the AutoNode ApplyDesire reconciler.
// It follows the same NewClusterWatchingController wiring as the other
// cluster-scoped kube-applier-writing controllers (e.g. backups) so its
// resync cadence and ReadDesire-triggered requeue behavior match the rest
// of the pipeline.
func NewAutoNodeEnablerController(
	cosmosClient corecosmosstorage.ResourcesDBClient,
	kubeApplierDBClients kubeappliercosmosstorage.KubeApplierDBClients,
	informers coreinformers.BackendInformers,
	kubeApplierInformers *unionkubeapplierinformers.UnionKubeApplierInformers,
	hostedClusterNamespaceEnvIdentifier string,
) controllerutils.Controller {
	_, clusterLister := informers.Clusters()
	_, serviceProviderClusterLister := informers.ServiceProviderClusters()
	_, readDesireLister := kubeApplierInformers.ReadDesires()
	applyDesireInformer, applyDesireLister := kubeApplierInformers.ApplyDesires()

	syncer := &autoNodeSyncer{
		clusterLister:                       clusterLister,
		serviceProviderClusterLister:        serviceProviderClusterLister,
		readDesireLister:                    readDesireLister,
		applyDesireLister:                   applyDesireLister,
		kubeApplierDBClients:                kubeApplierDBClients,
		hostedClusterNamespaceEnvIdentifier: hostedClusterNamespaceEnvIdentifier,
	}

	controller := controllerutils.NewClusterWatchingController(
		AutoNodeEnablerControllerName,
		cosmosClient,
		informers,
		kubeApplierInformers,
		5*time.Minute,
		syncer,
	)

	if kubeApplierInformers != nil {
		// React to ApplyDesire updates directly (e.g. an out-of-band edit)
		// so drift is corrected promptly rather than waiting for the next
		// resync cycle. Mirrors the backup-schedule controller's wiring.
		if err := controller.QueueForInformers(5*time.Minute, applyDesireInformer); err != nil {
			panic(err) // coding error
		}
	}

	return controller
}

func (s *autoNodeSyncer) SyncOnce(ctx context.Context, key controllerutils.HCPClusterKey) error {
	logger := utils.LoggerFromContext(ctx).WithValues(utils.LogValues{}.
		AddSubscriptionID(key.SubscriptionID).
		AddResourceGroup(key.ResourceGroupName).
		AddHCPClusterName(key.HCPClusterName)...)
	ctx = utils.ContextWithLogger(ctx, logger)

	existingCluster, err := s.clusterLister.Get(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if cosmosstorageutils.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get cached Cluster: %w", err))
	}
	// No coded disable/teardown path: once a cluster starts deleting, stop
	// touching it. Any existing ApplyDesire is swept by normal cluster-child
	// cleanup like the rest of the cluster-scoped kube-applier documents (to
	// be verified per the manual retirement runbook, not assumed here).
	if existingCluster.ServiceProviderProperties.DeletionTimestamp != nil {
		return nil
	}

	serviceProviderCluster, err := s.serviceProviderClusterLister.Get(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if cosmosstorageutils.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get cached ServiceProviderCluster: %w", err))
	}

	if serviceProviderCluster.Spec.DesiredAutoNodeEnabled == nil || !*serviceProviderCluster.Spec.DesiredAutoNodeEnabled {
		return nil
	}

	clientID := serviceProviderCluster.Spec.DesiredAutoNodeKarpenterAzureClientID
	if clientID == nil || *clientID == "" {
		// AutoNode is requested but the Azure managed-identity client ID
		// hasn't been supplied yet. hypershift's KarpenterAzureConfig.ClientID
		// is a required field once platform=Azure, so applying without it
		// would either be rejected by the kube-apiserver's CRD validation or
		// leave the karpenter-operator hard-erroring. Wait for the admin
		// request to include it rather than applying a doomed-to-fail desire.
		logger.Info("AutoNode enabled but no Karpenter Azure client ID set yet; waiting")
		return nil
	}

	mcResourceID := serviceProviderCluster.Status.ManagementClusterResourceID
	if mcResourceID == nil {
		return nil
	}

	if existingCluster.ServiceProviderProperties.ClusterServiceID == nil {
		return nil
	}
	csClusterID := existingCluster.ServiceProviderProperties.ClusterServiceID.ID()

	csClusterDomainPrefix := existingCluster.CustomerProperties.DNS.BaseDomainPrefix
	if len(csClusterDomainPrefix) == 0 {
		return nil
	}

	// Precondition (per the design's debate-hardened guardrails): only
	// attempt to SSA a field onto the HostedCluster once we've observed
	// that the live object actually exists. GetCachedHostedClusterForCluster
	// returns (nil, nil) for both "ReadDesire not created yet" and
	// "ReadDesire exists but not yet observed" — both mean "not ready",
	// not an error.
	hostedCluster, err := kubeapplierhelpers.GetCachedHostedClusterForCluster(
		ctx, s.readDesireLister, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName,
	)
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get cached HostedCluster: %w", err))
	}
	if hostedCluster == nil {
		return nil
	}

	kubeApplierClient := s.kubeApplierDBClients.For(ctx, mcResourceID)
	if kubeApplierClient == nil {
		// Registry doesn't have an entry yet for this MC (e.g. the fleet
		// lister hasn't caught up). Skip and rely on retrigger. When the MC
		// document is registered but misconfigured (e.g. missing its
		// kube-applier container name), For() surfaces that loudly.
		return nil
	}
	applyDesireCRUD, err := kubeApplierClient.ApplyDesiresForCluster(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if err != nil {
		return utils.TrackError(fmt.Errorf("get ApplyDesire CRUD: %w", err))
	}

	target := controllerutils.HostedClusterTarget(s.hostedClusterNamespaceEnvIdentifier, csClusterID, csClusterDomainPrefix)
	desire, err := buildAutoNodeApplyDesire(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName, mcResourceID, target, *clientID)
	if err != nil {
		return err
	}

	if err := kubeapplierhelpers.EnsureApplyDesire(ctx, applyDesireCRUD, s.applyDesireLister, desire); err != nil {
		return err
	}

	return nil
}
