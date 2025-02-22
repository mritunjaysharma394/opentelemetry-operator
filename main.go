// Copyright The OpenTelemetry Authors
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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/pflag"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	otelv1alpha1 "github.com/open-telemetry/opentelemetry-operator/apis/v1alpha1"
	"github.com/open-telemetry/opentelemetry-operator/controllers"
	"github.com/open-telemetry/opentelemetry-operator/internal/config"
	"github.com/open-telemetry/opentelemetry-operator/internal/version"
	"github.com/open-telemetry/opentelemetry-operator/internal/webhookhandler"
	"github.com/open-telemetry/opentelemetry-operator/pkg/autodetect"
	"github.com/open-telemetry/opentelemetry-operator/pkg/collector/upgrade"
	"github.com/open-telemetry/opentelemetry-operator/pkg/instrumentation"
	"github.com/open-telemetry/opentelemetry-operator/pkg/sidecar"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = k8sruntime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(otelv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	// registers any flags that underlying libraries might use
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)

	v := version.Get()

	// add flags related to this operator
	var metricsAddr string
	var enableLeaderElection bool
	var collectorImage string
	var targetAllocatorImage string
	var autoInstrumentationJava string
	pflag.StringVar(&metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	pflag.BoolVar(&enableLeaderElection, "enable-leader-election", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	pflag.StringVar(&collectorImage, "collector-image", fmt.Sprintf("otel/opentelemetry-collector:%s", v.OpenTelemetryCollector), "The default OpenTelemetry collector image. This image is used when no image is specified in the CustomResource.")
	pflag.StringVar(&targetAllocatorImage, "target-allocator-image", fmt.Sprintf("quay.io/opentelemetry/target-allocator:%s", v.TargetAllocator), "The default OpenTelemetry target allocator image. This image is used when no image is specified in the CustomResource.")
	pflag.StringVar(&autoInstrumentationJava, "auto-instrumentation-java-image", fmt.Sprintf("ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-java:%s", v.JavaAutoInstrumentation), "The default OpenTelemetry Java instrumentation image. This image is used when no image is specified in the CustomResource.")

	logger := zap.New(zap.UseFlagOptions(&opts))
	ctrl.SetLogger(logger)

	logger.Info("Starting the OpenTelemetry Operator",
		"opentelemetry-operator", v.Operator,
		"opentelemetry-collector", collectorImage,
		"opentelemetry-targetallocator", targetAllocatorImage,
		"auto-instrumentation-java", autoInstrumentationJava,
		"build-date", v.BuildDate,
		"go-version", v.Go,
		"go-arch", runtime.GOARCH,
		"go-os", runtime.GOOS,
	)

	restConfig := ctrl.GetConfigOrDie()

	// builds the operator's configuration
	ad, err := autodetect.New(restConfig)
	if err != nil {
		setupLog.Error(err, "failed to setup auto-detect routine")
		os.Exit(1)
	}

	cfg := config.New(
		config.WithLogger(ctrl.Log.WithName("config")),
		config.WithVersion(v),
		config.WithCollectorImage(collectorImage),
		config.WithTargetAllocatorImage(targetAllocatorImage),
		config.WithAutoDetect(ad),
	)

	pflag.CommandLine.AddFlagSet(cfg.FlagSet())

	pflag.Parse()

	watchNamespace, found := os.LookupEnv("WATCH_NAMESPACE")
	if found {
		setupLog.Info("watching namespace(s)", "namespaces", watchNamespace)
	} else {
		setupLog.Info("the env var WATCH_NAMESPACE isn't set, watching all namespaces")
	}

	mgrOptions := ctrl.Options{
		Scheme:             scheme,
		MetricsBindAddress: metricsAddr,
		Port:               9443,
		LeaderElection:     enableLeaderElection,
		LeaderElectionID:   "9f7554c3.opentelemetry.io",
		Namespace:          watchNamespace,
	}

	if strings.Contains(watchNamespace, ",") {
		mgrOptions.Namespace = ""
		mgrOptions.NewCache = cache.MultiNamespacedCacheBuilder(strings.Split(watchNamespace, ","))
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// run the auto-detect mechanism for the configuration
	err = mgr.Add(manager.RunnableFunc(func(_ context.Context) error {
		return cfg.StartAutoDetect()
	}))
	if err != nil {
		setupLog.Error(err, "failed to start the auto-detect mechanism")
	}

	// adds the upgrade mechanism to be executed once the manager is ready
	err = mgr.Add(manager.RunnableFunc(func(c context.Context) error {
		return upgrade.ManagedInstances(c, ctrl.Log.WithName("upgrade"), v, mgr.GetClient())
	}))
	if err != nil {
		setupLog.Error(err, "failed to upgrade managed instances")
	}

	if err = controllers.NewReconciler(controllers.Params{
		Client:   mgr.GetClient(),
		Log:      ctrl.Log.WithName("controllers").WithName("OpenTelemetryCollector"),
		Scheme:   mgr.GetScheme(),
		Config:   cfg,
		Recorder: mgr.GetEventRecorderFor("opentelemetry-operator"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "OpenTelemetryCollector")
		os.Exit(1)
	}

	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err = (&otelv1alpha1.OpenTelemetryCollector{}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "OpenTelemetryCollector")
			os.Exit(1)
		}

		mgr.GetWebhookServer().Register("/mutate-v1-pod", &webhook.Admission{
			Handler: webhookhandler.NewWebhookHandler(cfg, ctrl.Log.WithName("pod-webhook"), mgr.GetClient(),
				[]webhookhandler.PodMutator{
					sidecar.NewMutator(logger, cfg, mgr.GetClient()),
					instrumentation.NewMutator(logger, mgr.GetClient(), autoInstrumentationJava),
				}),
		})
	}
	// +kubebuilder:scaffold:builder

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
