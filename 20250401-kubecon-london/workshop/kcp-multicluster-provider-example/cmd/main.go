/*
Copyright 2025.

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
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.

	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	apisv1alpha1 "github.com/kcp-dev/kcp/sdk/apis/apis/v1alpha1"
	"github.com/kcp-dev/multicluster-provider/virtualworkspace"
	mcbuilder "github.com/multicluster-runtime/multicluster-runtime/pkg/builder"
	mcmanager "github.com/multicluster-runtime/multicluster-runtime/pkg/manager"
	mcreconcile "github.com/multicluster-runtime/multicluster-runtime/pkg/reconcile"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	cnpgapiv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"

	applicationapisv1alpha1 "github.com/kcp-dev/multicluster-provider/examples/crd/api/v1alpha1"
	"github.com/kcp-dev/multicluster-provider/examples/crd/internal/controller"
	// +kubebuilder:scaffold:imports
)

var (
	//scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(clientgoscheme.Scheme))
	utilruntime.Must(applicationapisv1alpha1.AddToScheme(clientgoscheme.Scheme))
	utilruntime.Must(cnpgapiv1.AddToScheme(clientgoscheme.Scheme))
	// MULTICLUSTER: This is where it differ from the default scaffold.
	utilruntime.Must(apisv1alpha1.AddToScheme(clientgoscheme.Scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var server string
	var providerKubeConfig string
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
	// MULTICLUSTER: This is where it differ from the default scaffold.
	flag.StringVar(&server, "server", "", "Override for kubeconfig server URL")

	flag.StringVar(&providerKubeConfig, "provider-kubeconfig", "", "The path to the kubeconfig file for the provider cluster.")

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
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(webhookCertPath, webhookCertName),
			filepath.Join(webhookCertPath, webhookCertKey),
		)
		if err != nil {
			setupLog.Error(err, "Failed to initialize webhook certificate watcher")
			os.Exit(1)
		}

		webhookTLSOpts = append(webhookTLSOpts, func(config *tls.Config) {
			config.GetCertificate = webhookCertWatcher.GetCertificate
		})
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: webhookTLSOpts,
	})

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.2/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.2/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			setupLog.Error(err, "to initialize metrics certificate watcher", "error", err)
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	// MULTICLUSTER: This is where it differ from the default scaffold.
	ctx := signals.SetupSignalHandler()

	cfg := ctrl.GetConfigOrDie()
	cfg = rest.CopyConfig(cfg)
	if server != "" {
		cfg.Host = server
	}

	var err error
	provider, err := virtualworkspace.New(cfg, &apisv1alpha1.APIBinding{}, virtualworkspace.Options{
		Scheme: clientgoscheme.Scheme,
	})
	if err != nil {
		setupLog.Error(err, "unable to construct cluster provider")
		os.Exit(1)
	}

	providerKubeConfig = filepath.Clean(providerKubeConfig)
	if _, err := os.Stat(providerKubeConfig); err != nil {
		setupLog.Error(err, "unable to find provider kubeconfig")
		os.Exit(1)
	}
	config, err := clientcmd.BuildConfigFromFlags("", providerKubeConfig)
	if err != nil {
		setupLog.Error(err, "unable to build provider kubeconfig")
		os.Exit(1)
	}

	providerClusterDynamicClient, err := client.New(config, client.Options{
		Scheme: clientgoscheme.Scheme,
	})
	if err != nil {
		setupLog.Error(err, "unable to create dynamic client")
		os.Exit(1)
	}

	mgr, err := mcmanager.New(cfg, provider, ctrl.Options{
		Scheme:                 clientgoscheme.Scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "e3eac106.contrib.kcp.io",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to set up overall controller manager")
		os.Exit(1)
	}

	if err := mcbuilder.ControllerManagedBy(mgr).
		Named("kcp-applications-controller").
		For(&applicationapisv1alpha1.Application{}).
		Complete(mcreconcile.Func(
			func(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
				log := log.FromContext(ctx).WithValues("cluster", req.ClusterName)
				log.Info("Reconciling Application")

				cl, err := mgr.GetCluster(ctx, req.ClusterName)
				if err != nil {
					return reconcile.Result{}, fmt.Errorf("failed to get cluster: %w", err)
				}
				client := cl.GetClient()

				reconciler := &controller.ApplicationReconciler{
					Client:         client,
					Scheme:         cl.GetScheme(),
					ProviderClient: providerClusterDynamicClient,
				}
				return reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: req.NamespacedName})
			},
		)); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Application")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	// TODO(mjudeikis): This needs to be implemented in mcmanager.
	// if metricsCertWatcher != nil {
	//	setupLog.Info("Adding metrics certificate watcher to manager")
	//	if err := mgr.Add(metricsCertWatcher); err != nil {
	//		setupLog.Error(err, "unable to add metrics certificate watcher to manager")
	//		os.Exit(1)
	//	}
	// }
	//
	// if webhookCertWatcher != nil {
	//	setupLog.Info("Adding webhook certificate watcher to manager")
	//	if err := mgr.Add(webhookCertWatcher); err != nil {
	//		setupLog.Error(err, "unable to add webhook certificate watcher to manager")
	//		os.Exit(1)
	//	}
	// }

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	if provider != nil {
		setupLog.Info("Starting provider")
		go func() {
			if err := provider.Run(ctx, mgr); err != nil {
				setupLog.Error(err, "unable to run provider")
				os.Exit(1)
			}
		}()
	}

	setupLog.Info("starting manager", "server", server)
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
