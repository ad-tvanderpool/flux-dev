/*
Copyright 2026 Travis Vanderpool

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

// Command manager is the entrypoint of the flux-manifest-generator
// controller. It wires the controller-runtime manager, the GitOps Toolkit
// runtime options, and the artifact storage server.
package main

import (
	"fmt"
	"os"
	"time"

	flag "github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/utils/ptr"
	ctrlruntime "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcfg "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	gotkconfig "github.com/fluxcd/pkg/artifact/config"
	gotkdigest "github.com/fluxcd/pkg/artifact/digest"
	gotkserver "github.com/fluxcd/pkg/artifact/server"
	gotkstorage "github.com/fluxcd/pkg/artifact/storage"
	gotkacl "github.com/fluxcd/pkg/runtime/acl"
	gotkclient "github.com/fluxcd/pkg/runtime/client"
	gotkctrl "github.com/fluxcd/pkg/runtime/controller"
	gotkevents "github.com/fluxcd/pkg/runtime/events"
	gotkjitter "github.com/fluxcd/pkg/runtime/jitter"
	gotkelection "github.com/fluxcd/pkg/runtime/leaderelection"
	gotklogger "github.com/fluxcd/pkg/runtime/logger"
	gotkpprof "github.com/fluxcd/pkg/runtime/pprof"
	gotkprobes "github.com/fluxcd/pkg/runtime/probes"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"k8s.io/client-go/tools/record"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
	"github.com/tvanderpool/flux-manifest-generator/internal/controller"
	// +kubebuilder:scaffold:imports
)

const controllerName = "flux-manifest-generator"

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrlruntime.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(sourcev1.AddToScheme(scheme))
	utilruntime.Must(mgapi.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var (
		metricsAddr           string
		healthAddr            string
		eventsAddr            string
		concurrent            int
		httpRetry             int
		reconciliationTimeout time.Duration
		requeueDependency     time.Duration

		// GitOps Toolkit (gotk) runtime options.
		// https://pkg.go.dev/github.com/fluxcd/pkg/runtime
		aclOptions            gotkacl.Options
		artifactOptions       gotkconfig.Options
		clientOptions         gotkclient.Options
		intervalJitterOptions gotkjitter.IntervalOptions
		leaderElectionOptions gotkelection.Options
		logOptions            gotklogger.Options
		rateLimiterOptions    gotkctrl.RateLimiterOptions
		watchOptions          gotkctrl.WatchOptions
	)

	flag.IntVar(&concurrent, "concurrent", 10,
		"The number of concurrent resource reconciles.")
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080",
		"The address the metric endpoint binds to.")
	flag.StringVar(&healthAddr, "health-addr", ":9440",
		"The address the health endpoint binds to.")
	flag.StringVar(&eventsAddr, "events-addr", "",
		"The address of the events receiver.")
	flag.IntVar(&httpRetry, "http-retry", 9,
		"The maximum number of retries when failing to fetch artifacts over HTTP.")
	flag.DurationVar(&reconciliationTimeout, "reconciliation-timeout", 10*time.Minute,
		"The maximum duration of a reconciliation.")
	flag.DurationVar(&requeueDependency, "requeue-dependency", 5*time.Second,
		"The interval at which failing dependencies are reevaluated.")

	aclOptions.BindFlags(flag.CommandLine)
	artifactOptions.BindFlags(flag.CommandLine)
	clientOptions.BindFlags(flag.CommandLine)
	intervalJitterOptions.BindFlags(flag.CommandLine)
	leaderElectionOptions.BindFlags(flag.CommandLine)
	logOptions.BindFlags(flag.CommandLine)
	rateLimiterOptions.BindFlags(flag.CommandLine)
	watchOptions.BindFlags(flag.CommandLine)

	flag.Parse()

	ctrlruntime.SetLogger(gotklogger.NewLogger(logOptions))

	digestAlgo, err := gotkdigest.AlgorithmForName(artifactOptions.ArtifactDigestAlgo)
	if err != nil {
		setupLog.Error(err, "unable to configure canonical digest algorithm")
		os.Exit(1)
	}
	gotkdigest.Canonical = digestAlgo

	artifactStorage, err := gotkstorage.New(&artifactOptions)
	if err != nil {
		setupLog.Error(err, "unable to configure artifact storage")
		os.Exit(1)
	}
	setupLog.Info("storage setup for " + artifactStorage.BasePath)

	if err := intervalJitterOptions.SetGlobalJitter(nil); err != nil {
		setupLog.Error(err, "unable to set global jitter")
		os.Exit(1)
	}

	leaderElectionID := fmt.Sprintf("%s-%s", controllerName, "leader-election")

	mgrConfig := ctrlruntime.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress:   metricsAddr,
			ExtraHandlers: gotkpprof.GetHandlers(),
		},
		HealthProbeBindAddress:        healthAddr,
		LeaderElection:                leaderElectionOptions.Enable,
		LeaderElectionID:              leaderElectionID,
		LeaderElectionReleaseOnCancel: leaderElectionOptions.ReleaseOnCancel,
		LeaseDuration:                 &leaderElectionOptions.LeaseDuration,
		RenewDeadline:                 &leaderElectionOptions.RenewDeadline,
		Logger:                        ctrlruntime.Log,
		Controller: ctrlcfg.Controller{
			MaxConcurrentReconciles: concurrent,
			RecoverPanic:            ptr.To(true),
			ReconciliationTimeout:   reconciliationTimeout,
		},
		Client: ctrlclient.Options{
			Cache: &ctrlclient.CacheOptions{
				// Secrets and ConfigMaps are read on-demand to keep
				// the controller memory footprint small.
				DisableFor: []ctrlclient.Object{&corev1.Secret{}, &corev1.ConfigMap{}},
			},
		},
	}

	if !watchOptions.AllNamespaces {
		mgrConfig.Cache.DefaultNamespaces = map[string]ctrlcache.Config{
			os.Getenv(gotkctrl.EnvRuntimeNamespace): {},
		}
	}

	ctx := ctrlruntime.SetupSignalHandler()
	mgr, err := ctrlruntime.NewManager(ctrlruntime.GetConfigOrDie(), mgrConfig)
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Liveness passes from here, readiness blocks until leader election.
	gotkprobes.SetupChecks(mgr, setupLog)

	eventRecorder := mustSetupEventRecorder(mgr, eventsAddr, controllerName)

	if err := (&controller.ManifestGeneratorReconciler{
		ControllerName:            controllerName,
		Client:                    mgr.GetClient(),
		APIReader:                 mgr.GetAPIReader(),
		Scheme:                    mgr.GetScheme(),
		EventRecorder:             eventRecorder,
		Storage:                   artifactStorage,
		ArtifactFetchRetries:      httpRetry,
		DependencyRequeueInterval: requeueDependency,
		NoCrossNamespaceRefs:      aclOptions.NoCrossNamespaceRefs,
	}).SetupWithManager(ctx, mgr, controller.ManifestGeneratorReconcilerOptions{
		RateLimiter: gotkctrl.GetRateLimiter(rateLimiterOptions),
	}); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", mgapi.ManifestGeneratorKind)
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	go func() {
		<-mgr.Elected()

		setupLog.Info("starting storage server on " + artifactOptions.StorageAddress)
		if err := gotkserver.Start(ctx, &artifactOptions); err != nil {
			setupLog.Error(err, "artifact server error")
			os.Exit(1)
		}
	}()

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// mustSetupEventRecorder constructs an event recorder that posts to
// both the in-cluster event sink and an optional notification-controller
// HTTP endpoint, exiting if construction fails.
func mustSetupEventRecorder(mgr ctrlruntime.Manager, eventsAddr, controllerName string) record.EventRecorder {
	eventRecorder, err := gotkevents.NewRecorder(mgr, ctrlruntime.Log, eventsAddr, controllerName)
	if err != nil {
		setupLog.Error(err, "unable to create event recorder")
		os.Exit(1)
	}
	return eventRecorder
}
