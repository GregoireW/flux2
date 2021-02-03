/*
Copyright 2020 The Flux authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kustomizev1 "github.com/fluxcd/kustomize-controller/api/v1beta1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta1"

	"github.com/fluxcd/flux2/internal/flags"
	"github.com/fluxcd/flux2/internal/utils"
	"github.com/fluxcd/flux2/pkg/manifestgen/install"
	"github.com/fluxcd/flux2/pkg/manifestgen/sync"
)

var bootstrapCmd = &cobra.Command{
	Use:   "bootstrap",
	Short: "Bootstrap toolkit components",
	Long:  "The bootstrap sub-commands bootstrap the toolkit components on the targeted Git provider.",
}

type bootstrapFlags struct {
	version            string
	defaultComponents  []string
	extraComponents    []string
	registry           string
	imagePullSecret    string
	branch             string
	watchAllNamespaces bool
	networkPolicy      bool
	manifestsPath      string
	arch               flags.Arch
	logLevel           flags.LogLevel
	requiredComponents []string
	tokenAuth          bool
	clusterDomain      string
}

const (
	bootstrapDefaultBranch = "main"
)

var bootstrapArgs = NewBootstrapFlags()

func init() {
	bootstrapCmd.PersistentFlags().StringVarP(&bootstrapArgs.version, "version", "v", rootArgs.defaults.Version,
		"toolkit version")
	bootstrapCmd.PersistentFlags().StringSliceVar(&bootstrapArgs.defaultComponents, "components", rootArgs.defaults.Components,
		"list of components, accepts comma-separated values")
	bootstrapCmd.PersistentFlags().StringSliceVar(&bootstrapArgs.extraComponents, "components-extra", nil,
		"list of components in addition to those supplied or defaulted, accepts comma-separated values")
	bootstrapCmd.PersistentFlags().StringVar(&bootstrapArgs.registry, "registry", "ghcr.io/fluxcd",
		"container registry where the toolkit images are published")
	bootstrapCmd.PersistentFlags().StringVar(&bootstrapArgs.imagePullSecret, "image-pull-secret", "",
		"Kubernetes secret name used for pulling the toolkit images from a private registry")
	bootstrapCmd.PersistentFlags().Var(&bootstrapArgs.arch, "arch", bootstrapArgs.arch.Description())
	bootstrapCmd.PersistentFlags().StringVar(&bootstrapArgs.branch, "branch", bootstrapDefaultBranch,
		"default branch (for GitHub this must match the default branch setting for the organization)")
	bootstrapCmd.PersistentFlags().BoolVar(&bootstrapArgs.watchAllNamespaces, "watch-all-namespaces", true,
		"watch for custom resources in all namespaces, if set to false it will only watch the namespace where the toolkit is installed")
	bootstrapCmd.PersistentFlags().BoolVar(&bootstrapArgs.networkPolicy, "network-policy", true,
		"deny ingress access to the toolkit controllers from other namespaces using network policies")
	bootstrapCmd.PersistentFlags().BoolVar(&bootstrapArgs.tokenAuth, "token-auth", false,
		"when enabled, the personal access token will be used instead of SSH deploy key")
	bootstrapCmd.PersistentFlags().Var(&bootstrapArgs.logLevel, "log-level", bootstrapArgs.logLevel.Description())
	bootstrapCmd.PersistentFlags().StringVar(&bootstrapArgs.manifestsPath, "manifests", "", "path to the manifest directory")
	bootstrapCmd.PersistentFlags().StringVar(&bootstrapArgs.clusterDomain, "cluster-domain", rootArgs.defaults.ClusterDomain, "internal cluster domain")
	bootstrapCmd.PersistentFlags().MarkHidden("manifests")
	bootstrapCmd.PersistentFlags().MarkDeprecated("arch", "multi-arch container image is now available for AMD64, ARMv7 and ARM64")
	rootCmd.AddCommand(bootstrapCmd)
}

func NewBootstrapFlags() bootstrapFlags {
	return bootstrapFlags{
		logLevel:           flags.LogLevel(rootArgs.defaults.LogLevel),
		requiredComponents: []string{"source-controller", "kustomize-controller"},
	}
}

func bootstrapComponents() []string {
	return append(bootstrapArgs.defaultComponents, bootstrapArgs.extraComponents...)
}

func bootstrapValidate() error {
	components := bootstrapComponents()
	for _, component := range bootstrapArgs.requiredComponents {
		if !utils.ContainsItemString(components, component) {
			return fmt.Errorf("component %s is required", component)
		}
	}

	if err := utils.ValidateComponents(components); err != nil {
		return err
	}

	return nil
}

func generateInstallManifests(targetPath, namespace, tmpDir string, localManifests string) (string, error) {
	opts := install.Options{
		BaseURL:                localManifests,
		Version:                bootstrapArgs.version,
		Namespace:              rootArgs.namespace,
		Components:             bootstrapComponents(),
		Registry:               bootstrapArgs.registry,
		ImagePullSecret:        bootstrapArgs.imagePullSecret,
		WatchAllNamespaces:     bootstrapArgs.watchAllNamespaces,
		NetworkPolicy:          bootstrapArgs.networkPolicy,
		LogLevel:               bootstrapArgs.logLevel.String(),
		NotificationController: rootArgs.defaults.NotificationController,
		ManifestFile:           rootArgs.defaults.ManifestFile,
		Timeout:                rootArgs.timeout,
		TargetPath:             targetPath,
		ClusterDomain:          bootstrapArgs.clusterDomain,
	}

	if localManifests == "" {
		opts.BaseURL = rootArgs.defaults.BaseURL
	}

	output, err := install.Generate(opts)
	if err != nil {
		return "", fmt.Errorf("generating install manifests failed: %w", err)
	}

	filePath, err := output.WriteFile(tmpDir)
	if err != nil {
		return "", fmt.Errorf("generating install manifests failed: %w", err)
	}
	return filePath, nil
}

func applyInstallManifests(ctx context.Context, manifestPath string, components []string) error {
	kubectlArgs := []string{"apply", "-f", manifestPath}
	if _, err := utils.ExecKubectlCommand(ctx, utils.ModeOS, rootArgs.kubeconfig, rootArgs.kubecontext, kubectlArgs...); err != nil {
		return fmt.Errorf("install failed")
	}

	statusChecker := StatusChecker{}
	err := statusChecker.New(time.Second, rootArgs.timeout)
	if err != nil {
		return fmt.Errorf("install failed with: %v", err)
	}
	err = statusChecker.Assess(components...)
	if err != nil {
		return fmt.Errorf("install timed out waiting for rollout")
	}

	return nil
}

func generateSyncManifests(url, branch, name, namespace, targetPath, tmpDir string, interval time.Duration) (string, error) {
	opts := sync.Options{
		Name:         name,
		Namespace:    namespace,
		URL:          url,
		Branch:       branch,
		Interval:     interval,
		TargetPath:   targetPath,
		ManifestFile: sync.MakeDefaultOptions().ManifestFile,
	}

	manifest, err := sync.Generate(opts)
	if err != nil {
		return "", fmt.Errorf("generating install manifests failed: %w", err)
	}

	output, err := manifest.WriteFile(tmpDir)
	if err != nil {
		return "", err
	}
	outputDir := filepath.Dir(output)
	if err := utils.GenerateKustomizationYaml(outputDir); err != nil {
		return "", err
	}
	return outputDir, nil
}

func applySyncManifests(ctx context.Context, kubeClient client.Client, name, namespace, manifestsPath string) error {
	kubectlArgs := []string{"apply", "-k", manifestsPath}
	if _, err := utils.ExecKubectlCommand(ctx, utils.ModeStderrOS, rootArgs.kubeconfig, rootArgs.kubecontext, kubectlArgs...); err != nil {
		return err
	}

	logger.Waitingf("waiting for cluster sync")

	var gitRepository sourcev1.GitRepository
	if err := wait.PollImmediate(rootArgs.pollInterval, rootArgs.timeout,
		isGitRepositoryReady(ctx, kubeClient, types.NamespacedName{Name: name, Namespace: namespace}, &gitRepository)); err != nil {
		return err
	}

	var kustomization kustomizev1.Kustomization
	if err := wait.PollImmediate(rootArgs.pollInterval, rootArgs.timeout,
		isKustomizationReady(ctx, kubeClient, types.NamespacedName{Name: name, Namespace: namespace}, &kustomization)); err != nil {
		return err
	}

	return nil
}

func shouldInstallManifests(ctx context.Context, kubeClient client.Client, namespace string) bool {
	namespacedName := types.NamespacedName{
		Namespace: namespace,
		Name:      namespace,
	}
	var kustomization kustomizev1.Kustomization
	if err := kubeClient.Get(ctx, namespacedName, &kustomization); err != nil {
		return true
	}

	return kustomization.Status.LastAppliedRevision == ""
}

func shouldCreateDeployKey(ctx context.Context, kubeClient client.Client, namespace string) bool {
	namespacedName := types.NamespacedName{
		Namespace: namespace,
		Name:      namespace,
	}

	var existing corev1.Secret
	if err := kubeClient.Get(ctx, namespacedName, &existing); err != nil {
		return true
	}
	return false
}

func generateDeployKey(ctx context.Context, kubeClient client.Client, url *url.URL, namespace string) (string, error) {
	pair, err := generateKeyPair(ctx, sourceArgs.GitKeyAlgorithm, sourceArgs.GitRSABits, sourceArgs.GitECDSACurve)
	if err != nil {
		return "", err
	}

	hostKey, err := scanHostKey(ctx, url)
	if err != nil {
		return "", err
	}

	secret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      namespace,
			Namespace: namespace,
		},
		StringData: map[string]string{
			"identity":     string(pair.PrivateKey),
			"identity.pub": string(pair.PublicKey),
			"known_hosts":  string(hostKey),
		},
	}
	if err := upsertSecret(ctx, kubeClient, secret); err != nil {
		return "", err
	}

	return string(pair.PublicKey), nil
}

func checkIfBootstrapPathDiffers(ctx context.Context, kubeClient client.Client, namespace string, path string) (string, bool) {
	namespacedName := types.NamespacedName{
		Name:      namespace,
		Namespace: namespace,
	}
	var fluxSystemKustomization kustomizev1.Kustomization
	err := kubeClient.Get(ctx, namespacedName, &fluxSystemKustomization)
	if err != nil {
		return "", false
	}
	if fluxSystemKustomization.Spec.Path == path {
		return "", false
	}

	return fluxSystemKustomization.Spec.Path, true
}
