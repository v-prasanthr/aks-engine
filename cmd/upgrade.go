// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT license.

package cmd

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"regexp"
	"strings"
	"time"

	"github.com/Azure/aks-engine/pkg/api"
	"github.com/Azure/aks-engine/pkg/api/common"
	"github.com/Azure/aks-engine/pkg/armhelpers"
	"github.com/Azure/aks-engine/pkg/armhelpers/utils"
	"github.com/Azure/aks-engine/pkg/engine"
	"github.com/Azure/aks-engine/pkg/helpers"
	"github.com/Azure/aks-engine/pkg/i18n"
	"github.com/Azure/aks-engine/pkg/operations/kubernetesupgrade"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/blang/semver"
	"github.com/leonelquinteros/gotext"
	"github.com/pkg/errors"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

const (
	upgradeName                     = "upgrade"
	upgradeShortDescription         = "Upgrade an existing AKS Engine-created Kubernetes cluster"
	upgradeLongDescription          = "Upgrade an existing AKS Engine-created Kubernetes cluster, one node at a time"
	smalldiskWindowsImageIdentifier = "smalldisk"
	ctrdWindowsImageIdentifier      = "ctrd"
)

type upgradeCmd struct {
	authProvider

	// user input
	resourceGroupName                        string
	apiModelPath                             string
	deploymentDirectory                      string
	currentVersion                           string
	upgradeVersion                           string
	location                                 string
	kubeconfigPath                           string
	timeoutInMinutes                         int
	cordonDrainTimeoutInMinutes              int
	force                                    bool
	controlPlaneOnly                         bool
	disableClusterInitComponentDuringUpgrade bool
	upgradeWindowsVHD                        bool

	// derived
	containerService    *api.ContainerService
	apiVersion          string
	client              armhelpers.AKSEngineClient
	locale              *gotext.Locale
	nameSuffix          string
	agentPoolsToUpgrade map[string]bool
	timeout             *time.Duration
	cordonDrainTimeout  *time.Duration
}

func newUpgradeCmd() *cobra.Command {
	uc := upgradeCmd{
		authProvider: &authArgs{},
	}

	upgradeCmd := &cobra.Command{
		Use:   upgradeName,
		Short: upgradeShortDescription,
		Long:  upgradeLongDescription,
		RunE:  uc.run,
	}

	f := upgradeCmd.Flags()
	f.StringVarP(&uc.location, "location", "l", "", "location the cluster is deployed in (required)")
	f.StringVarP(&uc.resourceGroupName, "resource-group", "g", "", "the resource group where the cluster is deployed (required)")
	f.StringVarP(&uc.apiModelPath, "api-model", "m", "", "path to the generated apimodel.json file")
	f.StringVar(&uc.deploymentDirectory, "deployment-dir", "", "the location of the output from `generate`")
	f.StringVarP(&uc.upgradeVersion, "upgrade-version", "k", "", "desired kubernetes version (required)")
	f.StringVarP(&uc.kubeconfigPath, "kubeconfig", "b", "", "the path of the kubeconfig file")
	f.IntVar(&uc.timeoutInMinutes, "vm-timeout", -1, "how long to wait for each vm to be upgraded in minutes")
	f.IntVar(&uc.cordonDrainTimeoutInMinutes, "cordon-drain-timeout", -1, "how long to wait for each vm to be cordoned in minutes")
	f.BoolVarP(&uc.force, "force", "f", false, "force upgrading the cluster to desired version. Allows same version upgrades and downgrades.")
	f.BoolVarP(&uc.controlPlaneOnly, "control-plane-only", "", false, "upgrade control plane VMs only, do not upgrade node pools")
	f.BoolVarP(&uc.upgradeWindowsVHD, "upgrade-windows-vhd", "", true, "upgrade image reference of the Windows nodes")
	addAuthFlags(uc.getAuthArgs(), f)

	_ = f.MarkDeprecated("deployment-dir", "deployment-dir is no longer required for scale or upgrade. Please use --api-model.")

	return upgradeCmd
}

func (uc *upgradeCmd) validate(cmd *cobra.Command) error {
	var err error

	uc.locale, err = i18n.LoadTranslations()
	if err != nil {
		return errors.Wrap(err, "error loading translation files")
	}

	if uc.resourceGroupName == "" {
		_ = cmd.Usage()
		return errors.New("--resource-group must be specified")
	}

	if uc.location == "" {
		_ = cmd.Usage()
		return errors.New("--location must be specified")
	}
	uc.location = helpers.NormalizeAzureRegion(uc.location)

	if uc.timeoutInMinutes != -1 {
		timeout := time.Duration(uc.timeoutInMinutes) * time.Minute
		uc.timeout = &timeout
	}

	if uc.cordonDrainTimeoutInMinutes != -1 {
		cordonDrainTimeout := time.Duration(uc.cordonDrainTimeoutInMinutes) * time.Minute
		uc.cordonDrainTimeout = &cordonDrainTimeout
	}

	if uc.upgradeVersion == "" {
		_ = cmd.Usage()
		return errors.New("--upgrade-version must be specified")
	}

	if uc.apiModelPath == "" && uc.deploymentDirectory == "" {
		_ = cmd.Usage()
		return errors.New("--api-model must be specified")
	}

	if uc.apiModelPath != "" && uc.deploymentDirectory != "" {
		_ = cmd.Usage()
		return errors.New("ambiguous, please specify only one of --api-model and --deployment-dir")
	}

	return nil
}

func (uc *upgradeCmd) loadCluster() error {
	var err error

	ctx, cancel := context.WithTimeout(context.Background(), armhelpers.DefaultARMOperationTimeout)
	defer cancel()

	// Load apimodel from the directory.
	if uc.apiModelPath == "" {
		uc.apiModelPath = filepath.Join(uc.deploymentDirectory, apiModelFilename)
	}

	if _, err = os.Stat(uc.apiModelPath); os.IsNotExist(err) {
		return errors.Errorf("specified api model does not exist (%s)", uc.apiModelPath)
	}

	apiloader := &api.Apiloader{
		Translator: &i18n.Translator{
			Locale: uc.locale,
		},
	}

	// Load the container service.
	uc.containerService, uc.apiVersion, err = apiloader.LoadContainerServiceFromFile(uc.apiModelPath, true, true, nil)
	if err != nil {
		return errors.Wrap(err, "error parsing the api model")
	}

	// Ensure there aren't known-breaking API model configurations
	if uc.containerService.Properties.MasterProfile.AvailabilityProfile == api.VirtualMachineScaleSets {
		return errors.Errorf("clusters with a VMSS control plane are not upgradable using `aks-engine upgrade`")
	}
	if uc.containerService.Properties.OrchestratorProfile != nil &&
		uc.containerService.Properties.OrchestratorProfile.KubernetesConfig != nil &&
		to.Bool(uc.containerService.Properties.OrchestratorProfile.KubernetesConfig.EnableEncryptionWithExternalKms) &&
		to.Bool(uc.containerService.Properties.OrchestratorProfile.KubernetesConfig.UseManagedIdentity) &&
		uc.containerService.Properties.OrchestratorProfile.KubernetesConfig.UserAssignedID == "" {
		return errors.Errorf("clusters with enableEncryptionWithExternalKms=true and system-assigned identity are not upgradable using `aks-engine upgrade`")
	}

	// Set 60 minutes cordonDrainTimeout for Azure Stack Cloud to give it enough time to move around resources during Node Drain,
	// especially disk detach/attach operations. We still honor the user's input.
	if uc.cordonDrainTimeout == nil && uc.containerService.Properties.IsAzureStackCloud() {
		cordonDrainTimeout := time.Duration(60) * time.Minute
		uc.cordonDrainTimeout = &cordonDrainTimeout
	}

	// Use the Windows VHD associated with the aks-engine version if upgradeWindowsVHD is set to "true"
	if uc.upgradeWindowsVHD && uc.containerService.Properties.WindowsProfile != nil {
		windowsProfile := uc.containerService.Properties.WindowsProfile
		if api.ImagePublisherAndOfferMatch(windowsProfile, api.AKSWindowsServer2019ContainerDOSImageConfig) && strings.Contains(windowsProfile.WindowsSku, ctrdWindowsImageIdentifier) {
			windowsProfile.ImageVersion = api.AKSWindowsServer2019ContainerDOSImageConfig.ImageVersion
			windowsProfile.WindowsSku = api.AKSWindowsServer2019ContainerDOSImageConfig.ImageSku
		} else if api.ImagePublisherAndOfferMatch(windowsProfile, api.AKSWindowsServer2019OSImageConfig) && strings.Contains(windowsProfile.WindowsSku, smalldiskWindowsImageIdentifier) {
			windowsProfile.ImageVersion = api.AKSWindowsServer2019OSImageConfig.ImageVersion
			windowsProfile.WindowsSku = api.AKSWindowsServer2019OSImageConfig.ImageSku
		} else if api.ImagePublisherAndOfferMatch(windowsProfile, api.WindowsServer2019OSImageConfig) {
			windowsProfile.ImageVersion = api.WindowsServer2019OSImageConfig.ImageVersion
			windowsProfile.WindowsSku = api.WindowsServer2019OSImageConfig.ImageSku
		}
	}

	// Update the masterProfile and agentPoolProfiles distro for AzureStackCloud to use aks-ubuntu-18.04 instead of aks-ubuntu-16.04
	if uc.containerService.Properties.IsAzureStackCloud() {
		if uc.containerService.Properties.MasterProfile.Distro == api.AKSUbuntu1604 {
			log.Infoln("Distro 'aks-ubuntu-16.04' is not longer supported on Azure Stack Hub, overwriting master profile distro to 'aks-ubuntu-20.04'")
			uc.containerService.Properties.MasterProfile.Distro = api.AKSUbuntu2004
		} else if uc.containerService.Properties.MasterProfile.Distro == api.AKSUbuntu1804 {
			log.Infoln("Distro 'aks-ubuntu-18.04' is not longer supported on Azure Stack Hub, overwriting master profile distro to 'aks-ubuntu-20.04'")
			uc.containerService.Properties.MasterProfile.Distro = api.AKSUbuntu2004
		}

		for _, app := range uc.containerService.Properties.AgentPoolProfiles {
			if app.Distro == api.AKSUbuntu1604 {
				log.Infoln(fmt.Sprintf("Distro 'aks-ubuntu-16.04' is not longer supported on Azure Stack Hub, overwriting agent pool profile %s distro to 'aks-ubuntu-20.04'", app.Name))
				app.Distro = api.AKSUbuntu2004
			} else if app.Distro == api.AKSUbuntu1804 {
				log.Infoln(fmt.Sprintf("Distro 'aks-ubuntu-18.04' is not longer supported on Azure Stack Hub, overwriting agent pool profile %s distro to 'aks-ubuntu-20.04'", app.Name))
				app.Distro = api.AKSUbuntu2004
			}
		}
	}

	// Enforce UseCloudControllerManager for Kubernetes 1.21+ on Azure Stack cloud
	if uc.containerService.Properties.IsAzureStackCloud() && common.IsKubernetesVersionGe(uc.upgradeVersion, "1.21.0") {
		log.Infoln("The in-tree cloud provider is not longer supported on Azure Stack Hub for v1.21+ clusters, overwriting UseCloudControllerManager to 'true'")
		uc.containerService.Properties.OrchestratorProfile.KubernetesConfig.UseCloudControllerManager = to.BoolPtr(true)
	}

	// Only containerd runtime is allowed for Kubernetes 1.24+ on Azure Stack cloud
	if uc.containerService.Properties.IsAzureStackCloud() && strings.EqualFold(uc.containerService.Properties.OrchestratorProfile.KubernetesConfig.ContainerRuntime, "docker") && common.IsKubernetesVersionGe(uc.upgradeVersion, "1.24.0") {
		log.Infoln("The docker runtime is no longer supported for v1.24+ clusters, overwriting ContainerRuntime to 'containerd'")
		uc.containerService.Properties.OrchestratorProfile.KubernetesConfig.ContainerRuntime = "containerd"
	}

	// The cluster-init component is a cluster create-only feature, temporarily disable if enabled
	if i := api.GetComponentsIndexByName(uc.containerService.Properties.OrchestratorProfile.KubernetesConfig.Components, common.ClusterInitComponentName); i > -1 {
		if uc.containerService.Properties.OrchestratorProfile.KubernetesConfig.Components[i].IsEnabled() {
			uc.disableClusterInitComponentDuringUpgrade = true
			uc.containerService.Properties.OrchestratorProfile.KubernetesConfig.Components[i].Enabled = to.BoolPtr(false)
		}
	}

	if uc.containerService.Properties.IsCustomCloudProfile() {
		if err = writeCustomCloudProfile(uc.containerService); err != nil {
			return errors.Wrap(err, "error writing custom cloud profile")
		}
		if err = uc.containerService.Properties.SetCustomCloudSpec(api.AzureCustomCloudSpecParams{
			IsUpgrade: true,
			IsScale:   false,
		}); err != nil {
			return errors.Wrap(err, "error parsing the api model")
		}
	}

	if err = uc.getAuthArgs().validateAuthArgs(); err != nil {
		return err
	}

	if uc.client, err = uc.getAuthArgs().getClient(); err != nil {
		return errors.Wrap(err, "failed to get client")
	}

	_, err = uc.client.EnsureResourceGroup(ctx, uc.resourceGroupName, uc.location, nil)
	if err != nil {
		return errors.Wrap(err, "error ensuring resource group")
	}

	err = uc.initialize()
	if err != nil {
		return errors.Wrap(err, "error validating the api model")
	}
	return nil
}

func (uc *upgradeCmd) validateTargetVersion() error {
	// Get available upgrades for container service.
	orchestratorInfo, err := api.GetOrchestratorVersionProfile(uc.containerService.Properties.OrchestratorProfile, uc.containerService.Properties.HasWindows(), uc.containerService.Properties.IsAzureStackCloud())
	if err != nil {
		return errors.Wrap(err, "error getting list of available upgrades")
	}

	found := false
	for _, up := range orchestratorInfo.Upgrades {
		if up.OrchestratorVersion == uc.upgradeVersion {
			found = true
			break
		}
	}
	if !found {
		return errors.Errorf("upgrading from Kubernetes version %s to version %s is not supported. To see a list of available upgrades, use 'aks-engine get-versions --version %s'", uc.containerService.Properties.OrchestratorProfile.OrchestratorVersion, uc.upgradeVersion, uc.containerService.Properties.OrchestratorProfile.OrchestratorVersion)
	}
	return nil
}

func (uc *upgradeCmd) initialize() error {
	if uc.containerService.Location == "" {
		uc.containerService.Location = uc.location
	} else if uc.containerService.Location != uc.location {
		return errors.New("--location does not match api model location")
	}

	// Validate semver compatibility
	_, err := semver.Make(uc.upgradeVersion)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("Invalid --upgrade-version value '%s', not a semver string", uc.upgradeVersion))
	}

	if !uc.force {
		err := uc.validateTargetVersion()
		if err != nil {
			return errors.Wrap(err, "Invalid upgrade target version. Consider using --force if you really want to proceed")
		}
	}
	uc.currentVersion = uc.containerService.Properties.OrchestratorProfile.OrchestratorVersion
	uc.containerService.Properties.OrchestratorProfile.OrchestratorVersion = uc.upgradeVersion

	//allows to identify VMs in the resource group that belong to this cluster.
	uc.nameSuffix = uc.containerService.Properties.GetClusterID()

	log.Infoln(fmt.Sprintf("Upgrading cluster with name suffix: %s", uc.nameSuffix))

	uc.agentPoolsToUpgrade = make(map[string]bool)
	uc.agentPoolsToUpgrade[kubernetesupgrade.MasterPoolName] = true
	for _, agentPool := range uc.containerService.Properties.AgentPoolProfiles {
		uc.agentPoolsToUpgrade[agentPool.Name] = true
	}
	return nil
}

func (uc *upgradeCmd) run(cmd *cobra.Command, args []string) error {
	err := uc.validate(cmd)
	if err != nil {
		return errors.Wrap(err, "validating upgrade command")
	}

	err = uc.loadCluster()
	if err != nil {
		return errors.Wrap(err, "loading existing cluster")
	}

	if uc.containerService.Properties.IsAzureStackCloud() {
		if err = uc.validateOSBaseImage(); err != nil {
			return errors.Wrapf(err, "validating OS base images required by %s", uc.apiModelPath)
		}
	}

	upgradeCluster := kubernetesupgrade.UpgradeCluster{
		Translator: &i18n.Translator{
			Locale: uc.locale,
		},
		Logger:             log.NewEntry(log.New()),
		Client:             uc.client,
		StepTimeout:        uc.timeout,
		CordonDrainTimeout: uc.cordonDrainTimeout,
	}

	upgradeCluster.ClusterTopology = kubernetesupgrade.ClusterTopology{}
	upgradeCluster.SubscriptionID = uc.getAuthArgs().SubscriptionID.String()
	upgradeCluster.ResourceGroup = uc.resourceGroupName
	upgradeCluster.DataModel = uc.containerService
	upgradeCluster.NameSuffix = uc.nameSuffix
	upgradeCluster.AgentPoolsToUpgrade = uc.agentPoolsToUpgrade
	upgradeCluster.Force = uc.force
	upgradeCluster.ControlPlaneOnly = uc.controlPlaneOnly

	var kubeConfig string
	if uc.kubeconfigPath != "" {
		var path string
		var content []byte
		path, err = filepath.Abs(uc.kubeconfigPath)
		if err != nil {
			return errors.Wrap(err, "reading --kubeconfig")
		}
		content, err = ioutil.ReadFile(path)
		if err != nil {
			return errors.Wrap(err, "reading --kubeconfig")
		}
		kubeConfig = string(content)
	} else {
		kubeConfig, err = engine.GenerateKubeConfig(uc.containerService.Properties, uc.location)
		if err != nil {
			return errors.Wrap(err, "generating kubeconfig")
		}
	}

	upgradeCluster.IsVMSSToBeUpgraded = isVMSSNameInAgentPoolsArray
	upgradeCluster.CurrentVersion = uc.currentVersion

	if err = upgradeCluster.UpgradeCluster(uc.client, kubeConfig, BuildTag); err != nil {
		return errors.Wrap(err, "upgrading cluster")
	}

	// Save the new apimodel to reflect the cluster's state.
	// Restore the original cluster-init component enabled value, if it was disabled during upgrade
	if uc.disableClusterInitComponentDuringUpgrade {
		if i := api.GetComponentsIndexByName(uc.containerService.Properties.OrchestratorProfile.KubernetesConfig.Components, common.ClusterInitComponentName); i > -1 {
			uc.containerService.Properties.OrchestratorProfile.KubernetesConfig.Components[i].Enabled = to.BoolPtr(true)
		}
	}
	apiloader := &api.Apiloader{
		Translator: &i18n.Translator{
			Locale: uc.locale,
		},
	}
	b, err := apiloader.SerializeContainerService(uc.containerService, uc.apiVersion)
	if err != nil {
		return err
	}

	f := helpers.FileSaver{
		Translator: &i18n.Translator{
			Locale: uc.locale,
		},
	}
	dir, file := filepath.Split(uc.apiModelPath)
	return f.SaveFile(dir, file, b)
}

// isVMSSNameInAgentPoolsArray is a helper func to filter out any VMSS in the cluster resource group
// that are not participating in the aks-engine-created Kubernetes cluster
func isVMSSNameInAgentPoolsArray(vmss string, cs *api.ContainerService) bool {
	for _, pool := range cs.Properties.AgentPoolProfiles {
		if pool.AvailabilityProfile == api.VirtualMachineScaleSets {
			if pool.OSType == api.Windows {
				re := regexp.MustCompile(`^[0-9]{4}k8s[0]+`)
				if re.FindString(vmss) != "" {
					return true
				}
			} else {
				if poolName, _, _ := utils.VmssNameParts(vmss); poolName == pool.Name {
					return true
				}
			}
		}
	}
	return false
}

// validateOSBaseImage checks if the OS image is available on the target cloud (ATM, Azure Stack only)
func (uc *upgradeCmd) validateOSBaseImage() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := armhelpers.ValidateRequiredImages(ctx, uc.location, uc.containerService.Properties, uc.client); err != nil {
		return errors.Wrap(err, "OS base image not available in target cloud")
	}
	return nil
}
