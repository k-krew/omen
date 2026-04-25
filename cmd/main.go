/*
Copyright 2026.

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
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	chaosv1alpha1 "github.com/k-krew/omen/api/v1alpha1"
	"github.com/k-krew/omen/internal/controller"
	omenwebhook "github.com/k-krew/omen/internal/webhook"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(chaosv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var webhookTimeout time.Duration
	var agentImage string
	var agentPort int
	var tlsOpts []func(*tls.Config)

	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.DurationVar(&webhookTimeout, "webhook-timeout", 10*time.Second,
		"Timeout for outgoing approval webhook HTTP requests.")
	// Default value for the agent image; overridden at release time by the Helm chart's values.yaml.
	flag.StringVar(&agentImage, "agent-image", "ghcr.io/k-krew/omen-agent:latest",
		"Container image for the omen-agent sidecar injected into target pods.")
	flag.IntVar(&agentPort, "agent-port", 9999,
		"Port the omen-agent sidecar listens on. Must not conflict with user application ports.")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	webhookServer := buildWebhookServer(webhookCertPath, webhookCertName, webhookCertKey, tlsOpts)
	metricsServerOptions := buildMetricsOptions(
		metricsAddr, secureMetrics, metricsCertPath, metricsCertName, metricsCertKey, tlsOpts,
	)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "7085bdfb.kreicer.dev",
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// Read the shared secret from the environment. The secret is generated by
	// Helm and mounted into the controller pod; the mutating webhook injects it
	// into agent sidecars so they can authenticate controller requests.
	secretToken := os.Getenv("OMEN_SECRET_TOKEN")

	// Pre-flight check: verify the agent image registry is reachable before
	// enabling sidecar injection. If the registry is unreachable (e.g. air-gapped
	// cluster) we disable injection so user pods are not bricked with ImagePullBackOff.
	injectionEnabled := checkRegistryReachable(agentImage)
	if !injectionEnabled {
		setupLog.Info("WARNING: agent image registry is not reachable — sidecar injection will be disabled",
			"image", agentImage)
	}

	if err := setupControllers(mgr, agentPort, secretToken, agentImage, injectionEnabled, webhookTimeout); err != nil {
		setupLog.Error(err, "Failed to register controllers or webhooks")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// buildWebhookServer constructs the webhook server, optionally configuring cert paths.
func buildWebhookServer(certPath, certName, certKey string, tlsOpts []func(*tls.Config)) webhook.Server {
	opts := webhook.Options{TLSOpts: tlsOpts}
	if len(certPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", certPath, "webhook-cert-name", certName, "webhook-cert-key", certKey)
		opts.CertDir = certPath
		opts.CertName = certName
		opts.KeyName = certKey
	}
	return webhook.NewServer(opts)
}

// buildMetricsOptions constructs the metrics server options, optionally configuring cert paths.
// More info:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/server
// - https://book.kubebuilder.io/reference/metrics.html
func buildMetricsOptions(
	addr string, secure bool, certPath, certName, certKey string, tlsOpts []func(*tls.Config),
) metricsserver.Options {
	opts := metricsserver.Options{
		BindAddress:   addr,
		SecureServing: secure,
		TLSOpts:       tlsOpts,
	}
	if secure {
		// FilterProvider protects the metrics endpoint with authn/authz.
		// The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/filters#WithAuthenticationAndAuthorization
		opts.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	if len(certPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", certPath, "metrics-cert-name", certName, "metrics-cert-key", certKey)
		opts.CertDir = certPath
		opts.CertName = certName
		opts.KeyName = certKey
	}
	return opts
}

// setupControllers registers all reconcilers and webhooks with the manager.
func setupControllers(
	mgr ctrl.Manager, agentPort int, secretToken, agentImage string, injectionEnabled bool, webhookTimeout time.Duration,
) error {
	if err := (&controller.ExperimentReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorder("experiment-controller"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("experiment controller: %w", err)
	}
	if err := (&controller.ExperimentRunReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		Recorder:       mgr.GetEventRecorder("experimentrun-controller"),
		WebhookTimeout: webhookTimeout,
		AgentPort:      agentPort,
		SecretToken:    secretToken,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("experimentrun controller: %w", err)
	}
	if err := omenwebhook.SetupExperimentWebhookWithManager(mgr); err != nil {
		return fmt.Errorf("experiment webhook: %w", err)
	}
	omenwebhook.SetupPodWebhookWithManager(mgr, &omenwebhook.PodMutator{
		Client:      mgr.GetClient(),
		AgentImage:  agentImage,
		AgentPort:   agentPort,
		SecretToken: secretToken,
		Enabled:     injectionEnabled,
	})
	return nil
}

// checkRegistryReachable does a best-effort TCP dial to the image registry host.
// Returns false if the registry is unreachable, true otherwise.
func checkRegistryReachable(image string) bool {
	host := parseRegistryHost(image)
	conn, err := net.DialTimeout("tcp", host, 5*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// parseRegistryHost extracts the registry host:port from an image reference.
// Falls back to Docker Hub if no explicit registry is present.
func parseRegistryHost(image string) string {
	// Strip tag/digest.
	ref := image
	for i, ch := range image {
		if ch == ':' || ch == '@' {
			firstSlash := -1
			for j := range i {
				if image[j] == '/' {
					firstSlash = j
					break
				}
			}
			if firstSlash < 0 || firstSlash > i {
				// no slash before the colon → e.g. "alpine:3.21" → Docker Hub
				break
			}
			ref = image[:i]
			break
		}
	}

	// If the first path component looks like a hostname, use it.
	parts := splitRef(ref)
	if len(parts) > 1 {
		host := parts[0]
		if containsDot(host) || containsColon(host) {
			if !containsColon(host) {
				host += ":443"
			}
			return host
		}
	}
	return "registry-1.docker.io:443"
}

func splitRef(s string) []string {
	var parts []string
	start := 0
	for i, ch := range s {
		if ch == '/' {
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func containsDot(s string) bool {
	for _, ch := range s {
		if ch == '.' {
			return true
		}
	}
	return false
}

func containsColon(s string) bool {
	for _, ch := range s {
		if ch == ':' {
			return true
		}
	}
	return false
}
