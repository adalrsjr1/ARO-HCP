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

package e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"

	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/api/metadataapi"
	"github.com/Azure/ARO-HCP/test/util/framework"
	"github.com/Azure/ARO-HCP/test/util/labels"
	"github.com/Azure/ARO-HCP/test/util/verifiers"
)

// This test only exercises the enablement signal (AFEC-gated tag ->
// ExperimentalFeatures.AutoNode projection, admit_cluster.go) and confirms
// the cluster still comes up healthy with it set. AutoNode's downstream
// pieces (a customer-supplied "autonode" data-plane operator identity per
// ClusterOperatorIdentifierAutoNode in
// internal/azure/cluster_scoped_identities_config.go, CS delivery of that
// identity, and the Karpenter delivery bridge) are not wired up yet, so
// there is no real Karpenter behavior to assert on here. Extend this test
// once that identity plumbing lands.
var _ = Describe("Customer", func() {
	It("should be able to enable AutoNode via the experimental tag for a cluster with version >= 4.22",
		labels.RequireNothing, labels.Medium, labels.Positive, labels.AroRpApiCompatible, labels.CreateCluster,
		labels.MIContainers(0),
		func(ctx context.Context) {
			const clusterName = "autonode-enable-422"

			tc := framework.NewTestContext()

			By("checking API version availability")
			apiAvailable, err := tc.IsHCPAPIVersionAvailable(ctx, "2026-06-30-preview")
			Expect(err).NotTo(HaveOccurred(), "failed to check API version availability")
			if !apiAvailable {
				if time.Now().After(framework.V20260630PreviewDeploymentDeadline) {
					Fail(fmt.Sprintf("API version 2026-06-30-preview should be fully available by %s", framework.V20260630PreviewDeploymentDeadline.Format(time.RFC3339)))
				}
				Skip("API version 2026-06-30-preview is not fully available in this environment")
			}

			if tc.UsePooledIdentities() {
				err = tc.AssignIdentityContainers(ctx, 0, framework.IdentityContainerAssignmentRetryInterval)
				Expect(err).NotTo(HaveOccurred(), "failed to assign pooled identity containers")
			}

			By("creating a resource group")
			resourceGroup, err := tc.NewResourceGroup(ctx, "autonode-enable", tc.Location())
			Expect(err).NotTo(HaveOccurred(), "failed to create resource group for AutoNode enablement test")

			By("creating cluster parameters with version 4.22 and the AutoNode experimental tag")
			clusterParams := framework.NewDefaultClusterParams20260630()
			clusterParams.ClusterName = clusterName
			clusterParams.OpenshiftVersionId = "4.22"
			// NOTE: The E2E subscription must have the ExperimentalReleaseFeatures AFEC
			// registered for this tag to be honored (see NewDefaultClusterParams20260630,
			// whose default tags rely on the same AFEC). Without it, admission silently
			// zeroes ExperimentalFeatures instead of rejecting the request
			// (mutateClusterExperimentalFeatures in internal/admission/admit_cluster.go),
			// so a missing AFEC registration would surface here as a failed tag
			// round-trip assertion below, not a clean create-time error.
			clusterParams.Tags[metadataapi.TagClusterAutoNode] = to.Ptr(string(coreapi.AutoNode))

			managedResourceGroupName := framework.SuffixName(*resourceGroup.Name, "-managed", 64)
			clusterParams.ManagedResourceGroupName = managedResourceGroupName

			By("creating customer resources")
			clusterParams, err = tc.CreateClusterCustomerResources20260630(ctx,
				resourceGroup,
				clusterParams,
				map[string]interface{}{},
				TestArtifactsFS,
				framework.RBACScopeResourceGroup,
			)
			Expect(err).NotTo(HaveOccurred(), "failed to create customer resources for AutoNode enablement cluster")

			By("creating the HCP cluster with the AutoNode tag set")
			err = tc.CreateHCPClusterFromParam20260630(
				ctx,
				GinkgoLogr,
				*resourceGroup.Name,
				clusterParams,
				nil, // imageDigestMirrors
				framework.ClusterCreationTimeout,
			)
			if isAPINotDeployedError(err) {
				if time.Now().Before(framework.V20260630PreviewDeploymentDeadline) {
					Skip(fmt.Sprintf("v20260630preview API not yet deployed; skipping until %s", framework.V20260630PreviewDeploymentDeadline.Format(time.RFC3339)))
				}
				Fail(fmt.Sprintf("v20260630preview API still not deployed as of %s deadline", framework.V20260630PreviewDeploymentDeadline.Format(time.RFC3339)))
			}
			Expect(err).NotTo(HaveOccurred(), "failed to create HCP cluster with the AutoNode tag set")

			By("verifying the AutoNode tag round-trips on the created cluster")
			actualHCPCluster, err := tc.Get20260630ClientFactoryOrDie(ctx).NewHcpOpenShiftClustersClient().Get(ctx, *resourceGroup.Name, clusterName, nil)
			Expect(err).NotTo(HaveOccurred(), "failed to get HCP cluster %s", clusterName)
			if tagValue := actualHCPCluster.Tags[metadataapi.TagClusterAutoNode]; tagValue == nil || *tagValue != string(coreapi.AutoNode) {
				subscriptionID, subErr := tc.SubscriptionID(ctx)
				if subErr != nil {
					subscriptionID = fmt.Sprintf("<unknown, failed to resolve: %s>", subErr)
				}
				Fail(fmt.Sprintf(
					"\n"+
						"=================================================================\n"+
						"AUTONODE TAG DID NOT ROUND-TRIP (got tag value: %v)\n"+
						"This is almost certainly NOT an AutoNode bug: it means the %q AFEC\n"+
						"flag is not registered on this test subscription (%s), so\n"+
						"mutateClusterExperimentalFeatures (internal/admission/admit_cluster.go)\n"+
						"silently zeroed ExperimentalFeatures instead of honoring the tag.\n"+
						"Fix by registering the flag on this subscription and re-running:\n"+
						"  az feature register --namespace Microsoft.RedHatOpenShift \\\n"+
						"    --name ExperimentalReleaseFeatures --subscription %s\n"+
						"=================================================================\n",
					tagValue, metadataapi.FeatureExperimentalReleaseFeatures, subscriptionID, subscriptionID,
				))
			}

			By("getting admin REST config")
			adminRESTConfig, err := tc.GetAdminRESTConfigForHCPCluster20260901(
				ctx,
				tc.Get20260901ClientFactoryOrDie(ctx).NewHcpOpenShiftClustersClient(),
				*resourceGroup.Name,
				clusterName,
				framework.GetAdminRESTConfigTimeout,
			)
			Expect(err).NotTo(HaveOccurred(), "failed to get admin REST config for HCP cluster %s", clusterName)

			By("verifying the cluster is healthy with AutoNode enabled")
			err = verifiers.VerifyHCPCluster(ctx, adminRESTConfig)
			Expect(err).NotTo(HaveOccurred(), "failed to verify HCP cluster %s is healthy", clusterName)
		})
})
