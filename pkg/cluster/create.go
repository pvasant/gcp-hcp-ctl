package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"github.com/go-logr/logr"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/infra/iam"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/infra/network"
	"github.com/openshift-online/gcp-hcp-ctl/pkg/platformapi"
	gcpv1 "github.com/openshift-online/gecko/platform-api/api/public/v1"
	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const maxInfraIDLength = 12

var (
	sanitizePattern     = regexp.MustCompile(`[^a-z0-9-]`)
	validInfraIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

func validateInfraID(id string) error {
	if len(id) > maxInfraIDLength {
		return fmt.Errorf("infra ID %q exceeds maximum length of %d characters", id, maxInfraIDLength)
	}
	if !validInfraIDPattern.MatchString(id) {
		return fmt.Errorf("infra ID %q is invalid: must start with a lowercase letter and contain only lowercase letters, digits, or hyphens", id)
	}
	return nil
}

func generateCompliantInfraID(clusterName string) (string, error) {
	sanitized := strings.ToLower(clusterName)
	sanitized = sanitizePattern.ReplaceAllString(sanitized, "")
	sanitized = strings.TrimLeft(sanitized, "-0123456789")
	sanitized = strings.Trim(sanitized, "-")

	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random suffix: %w", err)
	}
	suffix := hex.EncodeToString(b)

	maxPrefix := maxInfraIDLength - len(suffix) - 1
	if len(sanitized) > maxPrefix {
		sanitized = strings.TrimRight(sanitized[:maxPrefix], "-")
	}

	if sanitized == "" {
		sanitized = "hc"
	}

	return sanitized + "-" + suffix, nil
}

type createOptions struct {
	iamConfigFile     string
	networkConfigFile string
	setupInfra        bool
	endpointAccess    string
	version           string
	channelGroup      string
	dryRun            bool
	outputFmt         string
}

func newCreateCmd() *cobra.Command {
	opts := &createOptions{}

	cmd := &cobra.Command{
		Use:   "create <cluster-name>",
		Short: "Create a cluster",
		Long: `Create a cluster via the platform API server.

Two modes of operation:

  1. Config files: assemble from iam-config.json and network-config.json
     gcphcpctl cluster create my-cluster \
       --iam-config-file iam-config.json \
       --network-config-file network-config.json \
       --version 4.22.0-rc.5 --channel-group candidate

  2. Setup infra: provision IAM and network automatically, then create cluster
     gcphcpctl cluster create my-cluster \
       --setup-infra --project my-project \
       --version 4.22.0-rc.5 --channel-group candidate

Both --iam-config-file and --network-config-file are required in config-file mode.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("cluster name is required\n\nUsage: %s", cmd.UseLine())
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return opts.run(cmd, args[0])
		},
	}

	cmd.Flags().StringVar(&opts.iamConfigFile, "iam-config-file", "", "Path to IAM config JSON from 'gcphcpctl iam create'")
	cmd.Flags().StringVar(&opts.networkConfigFile, "network-config-file", "", "Path to network config JSON from 'gcphcpctl network create'")
	cmd.Flags().BoolVar(&opts.setupInfra, "setup-infra", false, "Automatically provision IAM and network infrastructure before creating cluster")
	cmd.Flags().StringVar(&opts.endpointAccess, "endpoint-access", "PublicAndPrivate", "API server endpoint access: Private or PublicAndPrivate")
	cmd.Flags().StringVar(&opts.version, "version", "", "OCP version (e.g. 4.22.0-rc.5)")
	cmd.Flags().StringVar(&opts.channelGroup, "channel-group", "stable", "Channel group: stable, fast, candidate, eus")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Show payload without creating")
	cmd.Flags().StringVarP(&opts.outputFmt, "output", "o", "text", "Output format: text, json, yaml")

	return cmd
}

func (o *createOptions) run(cmd *cobra.Command, clusterName string) error {
	if o.dryRun && o.setupInfra {
		return fmt.Errorf("--dry-run cannot be combined with --setup-infra because setup-infra has side effects")
	}

	switch o.endpointAccess {
	case "Private", "PublicAndPrivate":
	default:
		return fmt.Errorf("--endpoint-access must be one of: Private, PublicAndPrivate")
	}

	switch o.channelGroup {
	case "", "stable", "fast", "candidate", "eus":
	default:
		return fmt.Errorf("--channel-group must be one of: stable, fast, candidate, eus")
	}

	if o.version == "" {
		return fmt.Errorf("--version is required (e.g. --version 4.22.0)")
	}

	oidcBase, _ := cmd.Flags().GetString("oidc-endpoint")
	if oidcBase == "" {
		return fmt.Errorf("--oidc-endpoint is required (or set GCPHCPCTL_OIDC_ENDPOINT or oidc_endpoint in config)")
	}

	client := clientFromCmd(cmd)
	if err := validateVersion(cmd.Context(), client.Versions(), o.version, o.channelGroup); err != nil {
		return err
	}

	infraID, err := generateCompliantInfraID(clusterName)
	if err != nil {
		return fmt.Errorf("generating infra ID: %w", err)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Generated infra ID: %s\n", infraID)

	project, _ := cmd.Flags().GetString("project")
	region, _ := cmd.Flags().GetString("region")
	if region == "" {
		region = "us-central1"
	}

	bpo := buildPayloadOptions{
		clusterName:    clusterName,
		infraID:        infraID,
		projectID:      project,
		region:         region,
		endpointAccess: o.endpointAccess,
		oidcEndpoint:   oidcBase,
		version:        o.version,
		channelGroup:   o.channelGroup,
	}

	var cluster *gcpv1.Cluster

	switch {
	case o.iamConfigFile != "":
		if o.networkConfigFile == "" {
			return fmt.Errorf("--network-config-file is required with --iam-config-file")
		}
		cluster, err = buildPayloadFromConfigs(o.iamConfigFile, o.networkConfigFile, bpo)
		if err != nil {
			return err
		}

	case o.setupInfra:
		cluster, err = buildPayloadWithInfraSetup(cmd.Context(), cmd.ErrOrStderr(), bpo)
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("either --setup-infra or --iam-config-file and --network-config-file are required")
	}

	if o.dryRun {
		out := cmd.OutOrStdout()
		if _, err := fmt.Fprintln(out, "Dry run — would POST this payload:"); err != nil {
			return err
		}
		data, err := json.MarshalIndent(cluster, "", "  ")
		if err != nil {
			return fmt.Errorf("marshaling payload: %w", err)
		}
		_, err = fmt.Fprintln(out, string(data))
		return err
	}

	ns := platformapi.NamespaceForProject(bpo.projectID)
	created, err := client.Clusters().Create(cmd.Context(), ns, cluster)
	if err != nil {
		return fmt.Errorf("creating cluster: %w", err)
	}

	return printCluster(cmd.OutOrStdout(), created, o.outputFmt)
}

func validateVersion(ctx context.Context, versions platformapi.VersionInterface, version, channelGroup string) error {
	release, err := versions.Get(ctx, version)
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("version %q is not supported", version)
	}
	if err != nil {
		return fmt.Errorf("validating version %q: %w", version, err)
	}

	for _, group := range release.Spec.ChannelGroups {
		if group == channelGroup {
			return nil
		}
	}

	return fmt.Errorf("version %q is not available in channel group %q", version, channelGroup)
}

type buildPayloadOptions struct {
	clusterName    string
	infraID        string
	projectID      string
	region         string
	endpointAccess string
	oidcEndpoint   string
	version        string
	channelGroup   string
}

func (o buildPayloadOptions) issuerURL() string {
	return fmt.Sprintf("%s/%s", strings.TrimRight(o.oidcEndpoint, "/"), o.infraID)
}

func buildPayloadFromConfigs(iamConfigFile, networkConfigFile string, opts buildPayloadOptions) (*gcpv1.Cluster, error) {
	iamData, err := os.ReadFile(iamConfigFile)
	if err != nil {
		return nil, fmt.Errorf("reading IAM config: %w", err)
	}
	var iamConfig iam.CreateOutput
	if err := json.Unmarshal(iamData, &iamConfig); err != nil {
		return nil, fmt.Errorf("parsing IAM config: %w", err)
	}

	if iamConfig.InfraID != "" {
		if err := validateInfraID(iamConfig.InfraID); err != nil {
			return nil, fmt.Errorf("IAM config: %w", err)
		}
		opts.infraID = iamConfig.InfraID
	}
	if opts.projectID == "" {
		opts.projectID = iamConfig.ProjectID
	}

	networkData, err := os.ReadFile(networkConfigFile)
	if err != nil {
		return nil, fmt.Errorf("reading network config: %w", err)
	}
	var netConfig network.CreateOutput
	if err := json.Unmarshal(networkData, &netConfig); err != nil {
		return nil, fmt.Errorf("parsing network config: %w", err)
	}
	if netConfig.ProjectID != "" && netConfig.ProjectID != opts.projectID {
		return nil, fmt.Errorf("network config projectId %q does not match IAM config projectId %q", netConfig.ProjectID, opts.projectID)
	}
	if netConfig.Region != "" {
		opts.region = netConfig.Region
	}

	return assemblePayload(&iamConfig, &netConfig, opts)
}

func buildPayloadWithInfraSetup(ctx context.Context, w io.Writer, opts buildPayloadOptions) (*gcpv1.Cluster, error) {
	if opts.projectID == "" {
		return nil, fmt.Errorf("--project is required with --setup-infra (or set GCPHCPCTL_PROJECT or project in config)")
	}
	logger := logr.FromSlogHandler(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))

	fmt.Fprintln(w, ">>> Step 1: Setup IAM infrastructure")
	iamOpts := &iam.CreateOptions{
		ProjectID:     opts.projectID,
		InfraID:       opts.infraID,
		OIDCIssuerURL: opts.issuerURL(),
	}
	iamOutput, err := iamOpts.CreateIAM(ctx, logger)
	if err != nil {
		return nil, fmt.Errorf("IAM setup failed: %w", err)
	}
	fmt.Fprintf(w, "  IAM setup complete: %d service accounts created\n", len(iamOutput.ServiceAccounts))

	fmt.Fprintln(w, ">>> Step 2: Setup network infrastructure")
	networkOpts := &network.CreateOptions{
		ProjectID: opts.projectID,
		Region:    opts.region,
		InfraID:   opts.infraID,
	}
	networkOutput, err := networkOpts.CreateNetwork(ctx, logger)
	if err != nil {
		return nil, fmt.Errorf("network setup failed: %w", err)
	}
	fmt.Fprintf(w, "  Network setup complete: VPC=%s, Subnet=%s\n", networkOutput.NetworkName, networkOutput.SubnetName)

	fmt.Fprintln(w, ">>> Step 3: Building cluster payload")
	return assemblePayload(iamOutput, networkOutput, opts)
}

func validatePayloadInputs(projectID, region string, iamOutput *iam.CreateOutput, netOutput *network.CreateOutput) error {
	if projectID == "" {
		return fmt.Errorf("IAM config missing required field: projectId")
	}
	if region == "" {
		return fmt.Errorf("missing required field: region")
	}
	if iamOutput.ProjectNumber == "" {
		return fmt.Errorf("IAM config missing required field: projectNumber")
	}
	if iamOutput.WorkloadIdentityPool.PoolID == "" {
		return fmt.Errorf("IAM config missing required field: workloadIdentityPool.poolId")
	}
	if iamOutput.WorkloadIdentityPool.ProviderID == "" {
		return fmt.Errorf("IAM config missing required field: workloadIdentityPool.providerId")
	}
	requiredSAs := []string{"ctrlplane-op", "nodepool-mgmt", "cloud-controller", "gcp-pd-csi", "image-registry", "cloud-network"}
	for _, sa := range requiredSAs {
		if iamOutput.ServiceAccounts[sa] == "" {
			return fmt.Errorf("IAM config missing required service account: %s", sa)
		}
	}
	if netOutput.NetworkName == "" {
		return fmt.Errorf("network config missing required field: networkName")
	}
	if netOutput.SubnetName == "" {
		return fmt.Errorf("network config missing required field: subnetName")
	}
	return nil
}

func assemblePayload(iamOutput *iam.CreateOutput, netOutput *network.CreateOutput, opts buildPayloadOptions) (*gcpv1.Cluster, error) {
	if err := validatePayloadInputs(opts.projectID, opts.region, iamOutput, netOutput); err != nil {
		return nil, err
	}

	issuerURL := opts.issuerURL()

	cluster := &gcpv1.Cluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: gcpv1.GroupVersion.String(),
			Kind:       "Cluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: opts.clusterName,
		},
		Spec: gcpv1.ClusterSpec{
			InfraID:   opts.infraID,
			IssuerURL: issuerURL,
			Platform: gcpv1.ClusterPlatformSpec{
				Type: "GCP",
				GCP: &gcpv1.GCPClusterPlatform{
					ProjectID:      opts.projectID,
					Region:         opts.region,
					Network:        netOutput.NetworkName,
					Subnet:         netOutput.SubnetName,
					EndpointAccess: opts.endpointAccess,
					WorkloadIdentity: gcpv1.WorkloadIdentitySpec{
						ProjectNumber: iamOutput.ProjectNumber,
						PoolID:        iamOutput.WorkloadIdentityPool.PoolID,
						ProviderID:    iamOutput.WorkloadIdentityPool.ProviderID,
						ServiceAccountsRef: &gcpv1.ServiceAccountsRef{
							ControlPlaneEmail:    iamOutput.ServiceAccounts["ctrlplane-op"],
							NodePoolEmail:        iamOutput.ServiceAccounts["nodepool-mgmt"],
							CloudControllerEmail: iamOutput.ServiceAccounts["cloud-controller"],
							StorageEmail:         iamOutput.ServiceAccounts["gcp-pd-csi"],
							ImageRegistryEmail:   iamOutput.ServiceAccounts["image-registry"],
							NetworkEmail:         iamOutput.ServiceAccounts["cloud-network"],
						},
					},
				},
			},
			Release: gcpv1.ReleaseSpec{
				Version:      opts.version,
				ChannelGroup: opts.channelGroup,
			},
		},
	}

	return cluster, nil
}
