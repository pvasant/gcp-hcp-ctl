package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/openshift-online/gcp-hcp-ctl/pkg/infra/iam"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/infra/network"
	gcpv1 "github.com/openshift-online/gecko/platform-api/api/public/v1"
	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var infraIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*-[0-9a-f]{4}$`)

type fakeVersionClient struct {
	version *gcpv1.Version
	err     error
}

func (f *fakeVersionClient) Get(context.Context, string) (*gcpv1.Version, error) {
	return f.version, f.err
}

func TestValidateVersion(t *testing.T) {
	t.Run("When version belongs to the channel group it should succeed", func(t *testing.T) {
		versions := &fakeVersionClient{version: &gcpv1.Version{
			Spec: gcpv1.VersionSpec{ChannelGroups: []string{"fast", "stable"}},
		}}

		if err := validateVersion(context.Background(), versions, "4.22.13", "stable"); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
	})

	t.Run("When version does not belong to the channel group it should fail", func(t *testing.T) {
		versions := &fakeVersionClient{version: &gcpv1.Version{
			Spec: gcpv1.VersionSpec{ChannelGroups: []string{"candidate", "fast"}},
		}}

		err := validateVersion(context.Background(), versions, "4.22.14", "stable")
		if err == nil || !strings.Contains(err.Error(), `not available in channel group "stable"`) {
			t.Fatalf("expected channel-group validation error, got %v", err)
		}
	})

	t.Run("When version does not exist it should report that it is unsupported", func(t *testing.T) {
		versions := &fakeVersionClient{err: apierrors.NewNotFound(
			schema.GroupResource{Group: gcpv1.GroupVersion.Group, Resource: "versions"},
			"4.21.0",
		)}

		err := validateVersion(context.Background(), versions, "4.21.0", "stable")
		if err == nil || err.Error() != `version "4.21.0" is not supported` {
			t.Fatalf("expected unsupported-version error, got %v", err)
		}
	})

	t.Run("When the API request fails it should preserve the cause", func(t *testing.T) {
		versions := &fakeVersionClient{err: errors.New("API unavailable")}

		err := validateVersion(context.Background(), versions, "4.22.13", "stable")
		if err == nil || !strings.Contains(err.Error(), "API unavailable") {
			t.Fatalf("expected API error, got %v", err)
		}
	})
}

func TestCreateOptionsRunRejectsEmptyChannelGroup(t *testing.T) {
	opts := createOptions{
		endpointAccess: "PublicAndPrivate",
		version:        "4.22.13",
	}

	err := opts.run(&cobra.Command{}, "test-cluster")
	if err == nil || err.Error() != "--channel-group must be one of: stable, fast, candidate, eus" {
		t.Fatalf("expected empty channel-group validation error, got %v", err)
	}
}

func TestGenerateCompliantInfraID(t *testing.T) {
	t.Run("When given a simple cluster name it should produce a valid infra ID", func(t *testing.T) {
		id, err := generateCompliantInfraID("mycluster")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(id) > maxInfraIDLength {
			t.Errorf("infra ID %q exceeds max length %d", id, maxInfraIDLength)
		}
		if !infraIDPattern.MatchString(id) {
			t.Errorf("infra ID %q does not match expected pattern", id)
		}
	})

	t.Run("When given a long cluster name it should truncate and stay within max length", func(t *testing.T) {
		id, err := generateCompliantInfraID("this-is-a-very-long-cluster-name-that-exceeds-limits")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(id) > maxInfraIDLength {
			t.Errorf("infra ID %q exceeds max length %d", id, maxInfraIDLength)
		}
		if !infraIDPattern.MatchString(id) {
			t.Errorf("infra ID %q does not match expected pattern", id)
		}
	})

	t.Run("When given uppercase letters it should lowercase them", func(t *testing.T) {
		id, err := generateCompliantInfraID("MyCluster")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !infraIDPattern.MatchString(id) {
			t.Errorf("infra ID %q does not match expected pattern (should be lowercase)", id)
		}
	})

	t.Run("When given special characters it should strip them", func(t *testing.T) {
		id, err := generateCompliantInfraID("my_cluster!@#$%")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !infraIDPattern.MatchString(id) {
			t.Errorf("infra ID %q does not match expected pattern", id)
		}
	})

	t.Run("When name starts with digits it should strip leading digits", func(t *testing.T) {
		id, err := generateCompliantInfraID("123abc")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !infraIDPattern.MatchString(id) {
			t.Errorf("infra ID %q should start with a letter", id)
		}
	})

	t.Run("When name sanitizes to empty it should fall back to 'hc' prefix", func(t *testing.T) {
		id, err := generateCompliantInfraID("!!!")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(id) < 3 || id[:2] != "hc" {
			t.Errorf("expected infra ID to start with 'hc', got %q", id)
		}
	})

	t.Run("When called twice it should produce different IDs", func(t *testing.T) {
		id1, err := generateCompliantInfraID("cluster")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		id2, err := generateCompliantInfraID("cluster")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id1 == id2 {
			t.Errorf("expected unique IDs, both got %q", id1)
		}
	})
}

func TestValidateInfraID(t *testing.T) {
	t.Run("When given a valid infra ID it should return no error", func(t *testing.T) {
		for _, id := range []string{"abc", "my-infra", "a1b2c3", "a-1-b-2"} {
			if err := validateInfraID(id); err != nil {
				t.Errorf("expected %q to be valid, got error: %v", id, err)
			}
		}
	})

	t.Run("When infra ID is exactly max length it should return no error", func(t *testing.T) {
		id := "abcde-fghijk"
		if len(id) != maxInfraIDLength {
			t.Fatalf("test setup: expected length %d, got %d", maxInfraIDLength, len(id))
		}
		if err := validateInfraID(id); err != nil {
			t.Errorf("expected %q to be valid, got error: %v", id, err)
		}
	})

	t.Run("When infra ID exceeds max length it should return error", func(t *testing.T) {
		err := validateInfraID("this-is-too-long-infra-id")
		if err == nil {
			t.Error("expected error for infra ID exceeding max length")
		}
	})

	t.Run("When infra ID starts with a digit it should return error", func(t *testing.T) {
		err := validateInfraID("1abc")
		if err == nil {
			t.Error("expected error for infra ID starting with a digit")
		}
	})

	t.Run("When infra ID contains uppercase letters it should return error", func(t *testing.T) {
		err := validateInfraID("MyInfra")
		if err == nil {
			t.Error("expected error for infra ID with uppercase letters")
		}
	})

	t.Run("When infra ID is empty it should return error", func(t *testing.T) {
		err := validateInfraID("")
		if err == nil {
			t.Error("expected error for empty infra ID")
		}
	})
}

func validIAMOutput() *iam.CreateOutput {
	return &iam.CreateOutput{
		ProjectID:     "my-project",
		ProjectNumber: "123456789",
		InfraID:       "test-infra",
		WorkloadIdentityPool: iam.WorkloadIdentityConfig{
			PoolID:     "my-pool",
			ProviderID: "my-provider",
		},
		ServiceAccounts: map[string]string{
			"ctrlplane-op":     "ctrlplane@my-project.iam.gserviceaccount.com",
			"nodepool-mgmt":    "nodepool@my-project.iam.gserviceaccount.com",
			"cloud-controller": "cloud-ctrl@my-project.iam.gserviceaccount.com",
			"gcp-pd-csi":       "storage@my-project.iam.gserviceaccount.com",
			"image-registry":   "registry@my-project.iam.gserviceaccount.com",
			"cloud-network":    "network@my-project.iam.gserviceaccount.com",
		},
	}
}

func TestAssemblePayload(t *testing.T) {
	t.Run("When given valid inputs it should produce a correct cluster object", func(t *testing.T) {
		iamOutput := validIAMOutput()
		netOutput := &network.CreateOutput{NetworkName: "my-vpc", SubnetName: "my-subnet"}
		opts := buildPayloadOptions{
			clusterName:    "test-cluster",
			infraID:        "test-infra",
			projectID:      "my-project",
			region:         "us-central1",
			endpointAccess: "PublicAndPrivate",
			oidcEndpoint:   "https://oidc.example.com",
			version:        "4.22.0",
			channelGroup:   "candidate",
		}

		cluster, err := assemblePayload(iamOutput, netOutput, opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if cluster.Name != "test-cluster" {
			t.Errorf("expected name 'test-cluster', got %q", cluster.Name)
		}
		if cluster.Kind != "Cluster" {
			t.Errorf("expected kind 'Cluster', got %q", cluster.Kind)
		}
		if cluster.Spec.IssuerURL != "https://oidc.example.com/test-infra" {
			t.Errorf("expected issuerURL 'https://oidc.example.com/test-infra', got %q", cluster.Spec.IssuerURL)
		}
		if cluster.Spec.InfraID != "test-infra" {
			t.Errorf("expected infraID 'test-infra', got %q", cluster.Spec.InfraID)
		}
		if cluster.Spec.Release.Version != "4.22.0" {
			t.Errorf("expected version '4.22.0', got %q", cluster.Spec.Release.Version)
		}
		if cluster.Spec.Release.ChannelGroup != "candidate" {
			t.Errorf("expected channelGroup 'candidate', got %q", cluster.Spec.Release.ChannelGroup)
		}
		if cluster.Spec.Platform.GCP == nil {
			t.Fatal("expected GCP platform to be non-nil")
		}
		if cluster.Spec.Platform.GCP.ProjectID != "my-project" {
			t.Errorf("expected projectID 'my-project', got %q", cluster.Spec.Platform.GCP.ProjectID)
		}
		if cluster.Spec.Platform.GCP.Region != "us-central1" {
			t.Errorf("expected region 'us-central1', got %q", cluster.Spec.Platform.GCP.Region)
		}
		if cluster.Spec.Platform.GCP.Network != "my-vpc" {
			t.Errorf("expected network 'my-vpc', got %q", cluster.Spec.Platform.GCP.Network)
		}
		if cluster.Spec.Platform.GCP.WorkloadIdentity.ServiceAccountsRef == nil {
			t.Fatal("expected serviceAccountsRef to be non-nil")
		}
		if cluster.Spec.Platform.GCP.WorkloadIdentity.ServiceAccountsRef.ControlPlaneEmail != "ctrlplane@my-project.iam.gserviceaccount.com" {
			t.Errorf("unexpected controlPlane email: %q", cluster.Spec.Platform.GCP.WorkloadIdentity.ServiceAccountsRef.ControlPlaneEmail)
		}
	})

	t.Run("When network config is missing networkName it should return error", func(t *testing.T) {
		iamOutput := validIAMOutput()
		netOutput := &network.CreateOutput{SubnetName: "sub"}
		opts := buildPayloadOptions{clusterName: "cl", infraID: "cl", projectID: "proj", region: "us-east1", endpointAccess: "Private", oidcEndpoint: "https://oidc.test"}
		_, err := assemblePayload(iamOutput, netOutput, opts)
		if err == nil {
			t.Error("expected error for missing networkName")
		}
	})

	t.Run("When IAM config is missing required service account it should return error", func(t *testing.T) {
		iamOutput := &iam.CreateOutput{
			ProjectID: "proj", ProjectNumber: "999",
			WorkloadIdentityPool: iam.WorkloadIdentityConfig{PoolID: "p", ProviderID: "pr"},
			ServiceAccounts:      map[string]string{},
		}
		netOutput := &network.CreateOutput{NetworkName: "net", SubnetName: "sub"}
		opts := buildPayloadOptions{clusterName: "cl", infraID: "cl", projectID: "proj", region: "us-east1", endpointAccess: "Private", oidcEndpoint: "https://oidc.test"}
		_, err := assemblePayload(iamOutput, netOutput, opts)
		if err == nil {
			t.Error("expected error for missing service accounts")
		}
	})
}

func TestBuildPayloadFromConfigs(t *testing.T) {
	t.Run("When given valid config files it should assemble payload", func(t *testing.T) {
		dir := t.TempDir()

		iamConfig := validIAMOutput()
		iamConfig.InfraID = "my-infra"
		iamFile := filepath.Join(dir, "iam.json")
		iamData, err := json.Marshal(iamConfig)
		if err != nil {
			t.Fatalf("marshaling IAM config: %v", err)
		}
		if err := os.WriteFile(iamFile, iamData, 0644); err != nil {
			t.Fatalf("writing IAM config: %v", err)
		}

		networkConfig := map[string]string{
			"region":      "europe-west1",
			"networkName": "net-1",
			"subnetName":  "sub-1",
		}
		netFile := filepath.Join(dir, "net.json")
		netData, err := json.Marshal(networkConfig)
		if err != nil {
			t.Fatalf("marshaling network config: %v", err)
		}
		if err := os.WriteFile(netFile, netData, 0644); err != nil {
			t.Fatalf("writing network config: %v", err)
		}

		opts := buildPayloadOptions{
			clusterName:    "my-cluster",
			infraID:        "generated-id",
			region:         "us-central1",
			endpointAccess: "PublicAndPrivate",
			oidcEndpoint:   "https://oidc.test",
			version:        "4.21.0",
			channelGroup:   "stable",
		}

		cluster, err := buildPayloadFromConfigs(iamFile, netFile, opts)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if cluster.Name != "my-cluster" {
			t.Errorf("expected name 'my-cluster', got %q", cluster.Name)
		}
		if cluster.Spec.InfraID != "my-infra" {
			t.Errorf("expected infraID from IAM config 'my-infra' to override generated ID, got %q", cluster.Spec.InfraID)
		}
		if cluster.Spec.Platform.GCP.Region != "europe-west1" {
			t.Errorf("expected region from network config 'europe-west1', got %q", cluster.Spec.Platform.GCP.Region)
		}
	})

	t.Run("When network config projectId differs from IAM config it should return error", func(t *testing.T) {
		dir := t.TempDir()

		iamConfig := validIAMOutput()
		iamConfig.ProjectID = "project-a"
		iamFile := filepath.Join(dir, "iam.json")
		iamData, _ := json.Marshal(iamConfig)
		os.WriteFile(iamFile, iamData, 0644)

		netConfig := map[string]string{
			"projectId":   "project-b",
			"region":      "us-east1",
			"networkName": "net",
			"subnetName":  "sub",
		}
		netFile := filepath.Join(dir, "net.json")
		netData, _ := json.Marshal(netConfig)
		os.WriteFile(netFile, netData, 0644)

		opts := buildPayloadOptions{
			clusterName:  "cl",
			infraID:      "cl",
			projectID:    "project-a",
			region:       "us-east1",
			oidcEndpoint: "https://oidc.test",
		}

		_, err := buildPayloadFromConfigs(iamFile, netFile, opts)
		if err == nil {
			t.Error("expected error for mismatched project IDs")
		}
	})

	t.Run("When IAM config file does not exist it should return error", func(t *testing.T) {
		opts := buildPayloadOptions{clusterName: "cl", infraID: "cl", oidcEndpoint: "https://oidc.test"}
		_, err := buildPayloadFromConfigs("/nonexistent/iam.json", "", opts)
		if err == nil {
			t.Error("expected error for missing IAM config file")
		}
	})
}
