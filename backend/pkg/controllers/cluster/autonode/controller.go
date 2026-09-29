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

// Package autonode implements the backend controller that turns a cluster's
// AutoNode enablement into a kube-applier ApplyDesire that server-side-applies
// spec.autoNode onto the cluster's existing HostedCluster object, triggering
// the hypershift-operator to deploy the karpenter-operator.
//
// The trigger is Cluster.ServiceProviderProperties.ExperimentalFeatures.AutoNode,
// the sticky/immutable-post-create AFEC-gated feature flag. A cluster's
// AutoNode enablement DECISION is create-time and immutable: it is set once
// (via admission) and never changes for the life of the cluster, so there is
// no user-facing disable path.
//
// The DELIVERY MECHANISM this decision drives is, mechanically, an ordinary
// day-2 controller: it waits for the HostedCluster to actually exist (via the
// ReadDesire mirror) before it can SSA anything onto it, and it re-applies on
// every relevant resync - the same shape as autoscaler config or any other
// post-create HostedCluster field. Do not confuse "the decision is
// create-time-immutable" with "the write only ever happens once"; only the
// former is true.
//
// Decision-immutability does NOT mean there is nothing to tear down: the
// ApplyDesire this controller writes must be purged when the cluster is
// deleted, because the cluster-deletion precondition
// deletePreconditionAllApplyDesiresGone blocks forever on any remaining
// ApplyDesire, and the generic cluster-child cleanup controller deliberately
// skips desires tagged with an owning controller name (this one) - see
// syncDeletion below.
package autonode

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/ARO-HCP/backend/pkg/kubeapplierhelpers"
	"github.com/Azure/ARO-HCP/backend/pkg/utils/controllerutils"
	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/azure"
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
	imageOverrides                      ImageOverrides
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
	imageOverrides ImageOverrides,
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
		imageOverrides:                      imageOverrides,
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

	serviceProviderCluster, err := s.serviceProviderClusterLister.Get(ctx, key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if cosmosstorageutils.IsNotFoundError(err) {
		return nil
	}
	if err != nil {
		return utils.TrackError(fmt.Errorf("failed to get cached ServiceProviderCluster: %w", err))
	}

	// Handle deletion before the gates below. The cluster deletion controller
	// checks deletePreconditionAllApplyDesiresGone only after ClusterServiceID is
	// nil, while normal reconciliation returns early in that state. Moving this
	// below that gate would prevent syncDeletion from running and deadlock
	// deletion. The other gates can also become false after a desire is created,
	// so they must not block cleanup either.
	if needsDeletionWork(*existingCluster, *serviceProviderCluster) {
		return s.syncDeletion(ctx, key, serviceProviderCluster)
	}
	if existingCluster.ServiceProviderProperties.DeletionTimestamp != nil {
		// Deleting, but no management cluster was ever assigned, so nothing
		// could have been delivered. Nothing to tear down.
		return nil
	}

	// Trigger: the sticky, AFEC-gated, create-time-immutable experimental
	// feature.
	autoNodeRequested := existingCluster.ServiceProviderProperties.ExperimentalFeatures.AutoNode == coreapi.AutoNode
	if !autoNodeRequested {
		return nil
	}

	// ClientID is resolved from the cluster's own "autonode" data-plane
	// identity: it's the authoritative source, and can never go stale
	// relative to the identity actually federated for the cluster.
	clientID, ok := autoNodeKarpenterClientID(existingCluster, serviceProviderCluster)
	if !ok {
		// AutoNode is requested but the Azure managed-identity client ID
		// hasn't been resolved yet. hypershift's KarpenterAzureConfig.ClientID
		// is a required field once platform=Azure, so applying without it
		// would either be rejected by the kube-apiserver's CRD validation or
		// leave the karpenter-operator hard-erroring. Wait rather than
		// applying a doomed-to-fail desire.
		logger.Info("AutoNode enabled but no Karpenter Azure client ID resolved yet; waiting")
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

	// This SSA relies on two facts verified against real hypershift/CS
	// source, not assumed - re-verify both on any future hypershift/api
	// bump in this repo, or a bump to the pinned version CS uses:
	//   (a) hypershift-operator copies HostedCluster.spec.autoNode onto
	//       HostedControlPlane.spec.autoNode on every reconcile, not only at
	//       creation, so applying to an already-existing HostedCluster takes
	//       effect the same as applying at creation time.
	//   (b) every ARO-HCP HostedCluster already has Azure SubnetID and
	//       ResourceGroupName populated unconditionally (CS sets both as
	//       part of the mandatory Azure platform block), which the
	//       karpenter-operator's CPO adapter hard-requires once a Karpenter
	//       ClientID is set - so no additional guard for those fields is
	//       needed here.
	target := controllerutils.HostedClusterTarget(s.hostedClusterNamespaceEnvIdentifier, csClusterID, csClusterDomainPrefix)
	desire, err := buildAutoNodeApplyDesire(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName, mcResourceID, target, clientID, s.imageOverrides)
	if err != nil {
		return err
	}

	if err := kubeapplierhelpers.EnsureApplyDesire(ctx, applyDesireCRUD, s.applyDesireLister, desire); err != nil {
		return err
	}

	return nil
}

// needsDeletionWork reports whether the cluster is deleting and a management
// cluster was assigned to it, meaning an AutoNode ApplyDesire may have been
// delivered and needs tearing down. Mirrors backups' needsDeletionWork.
func needsDeletionWork(existingCluster coreapi.Cluster, serviceProviderCluster coreapi.ServiceProviderCluster) bool {
	if existingCluster.ServiceProviderProperties.DeletionTimestamp == nil {
		return false
	}
	return serviceProviderCluster.Status.ManagementClusterResourceID != nil
}

// syncDeletion tears down the AutoNode ApplyDesire for a deleting cluster.
//
// Purge the Cosmos document instead of marking the desire for deletion.
// Marking it would make kube-applier delete the target HostedCluster, although
// this desire owns only its spec.autoNode field. Cluster Service owns the
// HostedCluster lifecycle and handles its deletion. Purging is safe because
// this desire does not own a Kubernetes object; see PurgeApplyDesire's contract.
func (s *autoNodeSyncer) syncDeletion(ctx context.Context, key controllerutils.HCPClusterKey, serviceProviderCluster *coreapi.ServiceProviderCluster) error {
	kubeApplierClient := s.kubeApplierDBClients.For(ctx, serviceProviderCluster.Status.ManagementClusterResourceID)
	if kubeApplierClient == nil {
		// Registry doesn't have an entry yet for this MC. Skip and rely on
		// retrigger, same as the creation path above.
		return nil
	}
	applyDesireCRUD, err := kubeApplierClient.ApplyDesiresForCluster(key.SubscriptionID, key.ResourceGroupName, key.HCPClusterName)
	if err != nil {
		return utils.TrackError(fmt.Errorf("get ApplyDesire CRUD: %w", err))
	}

	if err := kubeapplierhelpers.PurgeApplyDesire(ctx, autoNodeApplyDesireName, applyDesireCRUD); err != nil {
		return err
	}
	utils.LoggerFromContext(ctx).Info("removed AutoNode ApplyDesire document for deleting cluster", "desire", autoNodeApplyDesireName)
	return nil
}

// autoNodeKarpenterClientID returns the resolved Azure client ID for the
// cluster's "autonode" data-plane operator identity, looked up on the
// ServiceProviderCluster status by the lowercased identity resource ID. The
// bool is false when the identity isn't configured on the cluster yet, or
// its client ID hasn't been resolved yet, or the most recent resolution
// attempt failed (RetrievalError set). Mirrors
// roleassignments.dataPlaneOperatorPrincipalID's lookup pattern.
func autoNodeKarpenterClientID(cluster *coreapi.Cluster, serviceProviderCluster *coreapi.ServiceProviderCluster) (string, bool) {
	identityResourceID, ok := cluster.CustomerProperties.Platform.OperatorsAuthentication.UserAssignedIdentities.DataPlaneOperators[string(azure.ClusterOperatorIdentifierAutoNode)]
	if !ok || identityResourceID == nil {
		return "", false
	}
	key := strings.ToLower(identityResourceID.String())
	identity, ok := serviceProviderCluster.Status.DataPlaneOperatorsManagedIdentities.Identities[key]
	if !ok || identity == nil || identity.RetrievalError != nil || identity.ClientID == nil || len(*identity.ClientID) == 0 {
		return "", false
	}
	return *identity.ClientID, true
}
