package platformapi

import (
	"strings"
	"testing"

	"github.com/openshift-online/gcp-hcp-ctl/pkg/auth"
	gcpv1 "github.com/openshift-online/gecko/platform-api/api/public/v1"
)

func TestNewClientValidation(t *testing.T) {
	t.Run("When token source is nil it should return error", func(t *testing.T) {
		_, err := NewClient("https://api.example.com", "my-project", nil)
		if err == nil {
			t.Fatal("expected error for nil token source")
		}
		if !strings.Contains(err.Error(), "token source is required") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("When project is empty it should return error", func(t *testing.T) {
		_, err := NewClient("https://api.example.com", "", auth.NewTokenSource(auth.PlatformAPIAudience))
		if err == nil {
			t.Fatal("expected error for empty project")
		}
		if !strings.Contains(err.Error(), "project is required") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("When endpoint is HTTP instead of HTTPS it should return error", func(t *testing.T) {
		_, err := NewClient("http://api.example.com", "my-project", auth.NewTokenSource(auth.PlatformAPIAudience))
		if err == nil {
			t.Fatal("expected error for HTTP endpoint")
		}
		if !strings.Contains(err.Error(), "HTTPS") {
			t.Errorf("expected HTTPS requirement in error, got: %v", err)
		}
	})

	t.Run("When endpoint is empty it should return error", func(t *testing.T) {
		_, err := NewClient("", "my-project", auth.NewTokenSource(auth.PlatformAPIAudience))
		if err == nil {
			t.Fatal("expected error for empty endpoint")
		}
	})
}

func TestSchemeRegistration(t *testing.T) {
	t.Run("When gecko public types are registered it should recognize Cluster", func(t *testing.T) {
		if !scheme.Recognizes(gcpv1.GroupVersion.WithKind("Cluster")) {
			t.Error("expected Cluster to be registered in scheme")
		}
	})

	t.Run("When gecko public types are registered it should recognize ClusterList", func(t *testing.T) {
		if !scheme.Recognizes(gcpv1.GroupVersion.WithKind("ClusterList")) {
			t.Error("expected ClusterList to be registered in scheme")
		}
	})

	t.Run("When gecko public types are registered it should recognize Version", func(t *testing.T) {
		if !scheme.Recognizes(gcpv1.GroupVersion.WithKind("Version")) {
			t.Error("expected Version to be registered in scheme")
		}
	})

	t.Run("When gecko public types are registered it should recognize VersionList", func(t *testing.T) {
		if !scheme.Recognizes(gcpv1.GroupVersion.WithKind("VersionList")) {
			t.Error("expected VersionList to be registered in scheme")
		}
	})
}

func TestNamespaceForProject(t *testing.T) {
	t.Run("When given a project ID it should return the project ID as namespace", func(t *testing.T) {
		if got := NamespaceForProject("ck-hcp-test"); got != "ck-hcp-test" {
			t.Errorf("expected 'ck-hcp-test', got %q", got)
		}
	})

	t.Run("When given a long project ID it should return it unchanged", func(t *testing.T) {
		if got := NamespaceForProject("dev-reg-us-c1-ckandag9545"); got != "dev-reg-us-c1-ckandag9545" {
			t.Errorf("expected 'dev-reg-us-c1-ckandag9545', got %q", got)
		}
	})
}
