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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"

	azcorearm "github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"

	"github.com/Azure/ARO-HCP/internal/api/coreapi"
	"github.com/Azure/ARO-HCP/internal/apitesting/coreapitesting"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstorage/corecosmosstorage"
	"github.com/Azure/ARO-HCP/internal/database/cosmosstoragetesting/corecosmosstoragetesting"
	"github.com/Azure/ARO-HCP/internal/utils"
)

func TestDesiredAutoNodeHandler(t *testing.T) {
	tests := []struct {
		name               string
		body               io.Reader
		skipResourceID     bool
		existingEnabled    *bool
		existingClientID   *string
		expectedStatusCode int
		expectedError      string
		expectedEnabled    *bool
		expectedClientID   *string
	}{
		{
			name:               "missing resource ID",
			body:               strings.NewReader(`{"enabled":true}`),
			skipResourceID:     true,
			expectedStatusCode: http.StatusBadRequest,
			expectedError:      "invalid resource identifier in request",
		},
		{
			name:               "invalid JSON body",
			body:               strings.NewReader(`{not json`),
			expectedStatusCode: http.StatusBadRequest,
			expectedError:      "invalid JSON body",
		},
		{
			name:               "omitted enabled rejected",
			body:               strings.NewReader(`{}`),
			expectedStatusCode: http.StatusBadRequest,
			expectedError:      "enabled must be set to true",
		},
		{
			name:               "explicit false rejected",
			body:               strings.NewReader(`{"enabled":false}`),
			expectedStatusCode: http.StatusBadRequest,
			expectedError:      "enabled must be set to true",
		},
		{
			name:               "enabled true creates SPC",
			body:               strings.NewReader(`{"enabled":true}`),
			expectedStatusCode: http.StatusOK,
			expectedEnabled:    ptr(true),
		},
		{
			name:               "enabled true is idempotent when already enabled",
			body:               strings.NewReader(`{"enabled":true}`),
			existingEnabled:    ptr(true),
			expectedStatusCode: http.StatusOK,
			expectedEnabled:    ptr(true),
		},
		{
			name:               "empty clientId rejected",
			body:               strings.NewReader(`{"enabled":true,"clientId":""}`),
			expectedStatusCode: http.StatusBadRequest,
			expectedError:      "clientId must not be empty",
		},
		{
			name:               "enabled true with clientId sets both fields",
			body:               strings.NewReader(`{"enabled":true,"clientId":"11111111-1111-1111-1111-111111111111"}`),
			expectedStatusCode: http.StatusOK,
			expectedEnabled:    ptr(true),
			expectedClientID:   ptr("11111111-1111-1111-1111-111111111111"),
		},
		{
			name:               "omitted clientId preserves existing clientId",
			body:               strings.NewReader(`{"enabled":true}`),
			existingEnabled:    ptr(true),
			existingClientID:   ptr("22222222-2222-2222-2222-222222222222"),
			expectedStatusCode: http.StatusOK,
			expectedEnabled:    ptr(true),
			expectedClientID:   ptr("22222222-2222-2222-2222-222222222222"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := utils.ContextWithLogger(context.Background(), testr.New(t))
			mockResourcesDBClient := corecosmosstoragetesting.NewMockResourcesDBClient()

			resourceID, err := azcorearm.ParseResourceID(coreapitesting.TestClusterResourceID)
			require.NoError(t, err)

			if tt.existingEnabled != nil {
				existing, err := corecosmosstorage.GetOrCreateServiceProviderCluster(ctx, mockResourcesDBClient, resourceID)
				require.NoError(t, err)
				existing.Spec.DesiredAutoNodeEnabled = tt.existingEnabled
				existing.Spec.DesiredAutoNodeKarpenterAzureClientID = tt.existingClientID
				_, err = mockResourcesDBClient.ServiceProviderClusters(resourceID.SubscriptionID, resourceID.ResourceGroupName, resourceID.Name).Replace(ctx, existing, nil)
				require.NoError(t, err)
			}

			handler := NewHCPDesiredAutoNodeHandler(mockResourcesDBClient)

			if !tt.skipResourceID {
				ctx = utils.ContextWithResourceID(ctx, resourceID)
			}

			req := httptest.NewRequest(http.MethodPut, "/autonode", tt.body)
			req = req.WithContext(ctx)
			recorder := httptest.NewRecorder()

			err = handler.ServeHTTP(recorder, req)

			if tt.expectedStatusCode >= 400 {
				if err == nil {
					t.Fatalf("expected error but got none")
				}
				var cloudErr *coreapi.CloudError
				if !errors.As(err, &cloudErr) {
					t.Fatalf("expected CloudError but got %T: %v", err, err)
				}
				if cloudErr.StatusCode != tt.expectedStatusCode {
					t.Errorf("expected status %d, got %d", tt.expectedStatusCode, cloudErr.StatusCode)
				}
				if tt.expectedError != "" && !strings.Contains(err.Error(), tt.expectedError) {
					t.Errorf("expected error containing %q, got %q", tt.expectedError, err.Error())
				}
				return
			}

			if err != nil {
				t.Fatalf("expected no error but got %v", err)
			}

			spc, err := mockResourcesDBClient.ServiceProviderClusters(resourceID.SubscriptionID, resourceID.ResourceGroupName, resourceID.Name).Get(ctx, coreapi.ServiceProviderClusterResourceName)
			require.NoError(t, err)

			var respBody desiredAutoNodeRequest
			require.NoError(t, json.NewDecoder(recorder.Body).Decode(&respBody))

			if spc.Spec.DesiredAutoNodeEnabled == nil || *spc.Spec.DesiredAutoNodeEnabled != *tt.expectedEnabled {
				t.Errorf("expected DesiredAutoNodeEnabled %v, got %v", *tt.expectedEnabled, spc.Spec.DesiredAutoNodeEnabled)
			}
			if respBody.Enabled == nil || *respBody.Enabled != *tt.expectedEnabled {
				t.Errorf("expected response enabled %v, got %v", *tt.expectedEnabled, respBody.Enabled)
			}
			if tt.expectedClientID == nil {
				if spc.Spec.DesiredAutoNodeKarpenterAzureClientID != nil {
					t.Errorf("expected DesiredAutoNodeKarpenterAzureClientID nil, got %v", *spc.Spec.DesiredAutoNodeKarpenterAzureClientID)
				}
			} else {
				if spc.Spec.DesiredAutoNodeKarpenterAzureClientID == nil || *spc.Spec.DesiredAutoNodeKarpenterAzureClientID != *tt.expectedClientID {
					t.Errorf("expected DesiredAutoNodeKarpenterAzureClientID %v, got %v", *tt.expectedClientID, spc.Spec.DesiredAutoNodeKarpenterAzureClientID)
				}
				if respBody.ClientID == nil || *respBody.ClientID != *tt.expectedClientID {
					t.Errorf("expected response clientId %v, got %v", *tt.expectedClientID, respBody.ClientID)
				}
			}
		})
	}
}

func TestDesiredAutoNodeHandler_PreservesOtherFields(t *testing.T) {
	ctx := utils.ContextWithLogger(context.Background(), testr.New(t))
	mockResourcesDBClient := corecosmosstoragetesting.NewMockResourcesDBClient()

	resourceID, err := azcorearm.ParseResourceID(coreapitesting.TestClusterResourceID)
	require.NoError(t, err)

	// Seed an SPC with a populated Status to confirm the handler does not stomp it.
	existing, err := corecosmosstorage.GetOrCreateServiceProviderCluster(ctx, mockResourcesDBClient, resourceID)
	require.NoError(t, err)
	mgmtResourceID, err := azcorearm.ParseResourceID("/subscriptions/" + coreapitesting.TestSubscriptionID + "/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/mc")
	require.NoError(t, err)
	existing.Status.ManagementClusterResourceID = mgmtResourceID
	_, err = mockResourcesDBClient.ServiceProviderClusters(resourceID.SubscriptionID, resourceID.ResourceGroupName, resourceID.Name).Replace(ctx, existing, nil)
	require.NoError(t, err)

	handler := NewHCPDesiredAutoNodeHandler(mockResourcesDBClient)
	ctx = utils.ContextWithResourceID(ctx, resourceID)

	req := httptest.NewRequest(http.MethodPut, "/autonode", strings.NewReader(`{"enabled":true}`))
	req = req.WithContext(ctx)
	recorder := httptest.NewRecorder()

	require.NoError(t, handler.ServeHTTP(recorder, req))

	spc, err := mockResourcesDBClient.ServiceProviderClusters(resourceID.SubscriptionID, resourceID.ResourceGroupName, resourceID.Name).Get(ctx, coreapi.ServiceProviderClusterResourceName)
	require.NoError(t, err)
	if spc.Status.ManagementClusterResourceID == nil || spc.Status.ManagementClusterResourceID.String() != mgmtResourceID.String() {
		t.Errorf("expected ManagementClusterResourceID preserved, got %v", spc.Status.ManagementClusterResourceID)
	}
	if spc.Spec.DesiredAutoNodeEnabled == nil || !*spc.Spec.DesiredAutoNodeEnabled {
		t.Errorf("expected DesiredAutoNodeEnabled true, got %v", spc.Spec.DesiredAutoNodeEnabled)
	}
}

func ptr[T any](v T) *T { return &v }
