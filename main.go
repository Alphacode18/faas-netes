// Copyright (c) Alex Ellis 2017. All rights reserved.
// Copyright (c) OpenFaaS Author(s) 2020. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	licensev1 "github.com/alexellis/jwt-license/pkg/v1"

	clientset "github.com/openfaas/faas-netes/pkg/client/clientset/versioned"
	informers "github.com/openfaas/faas-netes/pkg/client/informers/externalversions"
	v1 "github.com/openfaas/faas-netes/pkg/client/informers/externalversions/openfaas/v1"
	"github.com/openfaas/faas-netes/pkg/config"
	"github.com/openfaas/faas-netes/pkg/controller"
	"github.com/openfaas/faas-netes/pkg/handlers"
	"github.com/openfaas/faas-netes/pkg/k8s"
	"github.com/openfaas/faas-netes/pkg/server"
	"github.com/openfaas/faas-netes/pkg/signals"
	version "github.com/openfaas/faas-netes/version"
	faasProvider "github.com/openfaas/faas-provider"
	"github.com/openfaas/faas-provider/logs"
	"github.com/openfaas/faas-provider/proxy"
	providertypes "github.com/openfaas/faas-provider/types"

	kubeinformers "k8s.io/client-go/informers"
	v1apps "k8s.io/client-go/informers/apps/v1"
	v1core "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	glog "k8s.io/klog"

	// required to authenticate against GKE clusters
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"

	// required for updating and validating the CRD clientset
	_ "k8s.io/code-generator/cmd/client-gen/generators"
	// main.go:36:2: import "sigs.k8s.io/controller-tools/cmd/controller-gen" is a program, not an importable package
	// _ "sigs.k8s.io/controller-tools/cmd/controller-gen"
)

const (
	sku = "openfaas-pro"
)

var (
	PublicKey string
)

func hasProduct(sku string, list []string) bool {
	for _, p := range list {
		if p == sku {
			return true
		}
	}
	return false
}

func main() {
	var (
		kubeconfig,
		masterURL,
		license,
		licenseFile string
	)
	var (
		operator,
		verbose bool
	)

	if time.Now().After(time.Date(2022, time.March, 14, 0, 0, 0, 0, time.UTC)) {
		log.Fatalf("This demo has expired. Please email contact@openfaas.com for more information.")
	}

	flag.StringVar(&kubeconfig, "kubeconfig", "",
		"Path to a kubeconfig. Only required if out-of-cluster.")

	flag.BoolVar(&verbose, "verbose", false, "Print verbose config information")
	flag.StringVar(&masterURL, "master", "",
		"The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")

	flag.BoolVar(&operator, "operator", false, "Use the operator mode instead of faas-netes")
	flag.StringVar(&license, "license", "", "Literal value for the license")
	flag.StringVar(&licenseFile, "license-file", "", "Path to the file for the license")

	flag.Parse()

	log.Printf("Public key: %s", PublicKey)
	if len(license) == 0 && len(licenseFile) == 0 {
		log.Fatalf("A license is required via --license or --license-file")
	}

	sha, release := version.GetReleaseInfo()
	log.Printf("Version: %s\tcommit: %s\n", release, sha)

	if len(licenseFile) > 0 {
		res, err := ioutil.ReadFile(licenseFile)
		if err != nil {
			panic(err)
		}
		license = strings.TrimSpace(string(res))
	}

	if len(license) == 0 {
		panic("provide an argument of -license or -license-file to continue using this product")
	}

	// if len(PublicKey) > 0 {
	licenseToken, err := licensev1.LoadLicenseToken(license, PublicKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		fmt.Fprintf(os.Stderr, "PublicKey: %s\n", PublicKey)

		os.Exit(1)
	}

	validForProduct := hasProduct(sku, licenseToken.Products)
	if !validForProduct {
		panic(fmt.Errorf("this license is not valid for %q, contact support@openfaas.com", sku).Error())
	}

	log.Printf("Licensed to: %s <%s>, expires: %.0f day(s)\n",
		licenseToken.Name,
		licenseToken.Email,
		time.Until(licenseToken.Expires).Hours()/24)
	// }

	clientCmdConfig, err := clientcmd.BuildConfigFromFlags(masterURL, kubeconfig)
	if err != nil {
		log.Fatalf("Error building kubeconfig: %s", err.Error())
	}

	kubeconfigQPS := 100
	kubeconfigBurst := 250

	clientCmdConfig.QPS = float32(kubeconfigQPS)
	clientCmdConfig.Burst = kubeconfigBurst

	kubeClient, err := kubernetes.NewForConfig(clientCmdConfig)
	if err != nil {
		log.Fatalf("Error building Kubernetes clientset: %s", err.Error())
	}

	faasClient, err := clientset.NewForConfig(clientCmdConfig)
	if err != nil {
		log.Fatalf("Error building OpenFaaS clientset: %s", err.Error())
	}

	readConfig := config.ReadConfig{}
	osEnv := providertypes.OsEnv{}
	config, err := readConfig.Read(osEnv)

	if err != nil {
		log.Fatalf("Error reading config: %s", err.Error())
	}

	config.Fprint(verbose)

	deployConfig := k8s.DeploymentConfig{
		RuntimeHTTPPort: 8080,
		HTTPProbe:       config.HTTPProbe,
		SetNonRootUser:  config.SetNonRootUser,
		ReadinessProbe: &k8s.ProbeConfig{
			InitialDelaySeconds: int32(config.ReadinessProbeInitialDelaySeconds),
			TimeoutSeconds:      int32(config.ReadinessProbeTimeoutSeconds),
			PeriodSeconds:       int32(config.ReadinessProbePeriodSeconds),
		},
		LivenessProbe: &k8s.ProbeConfig{
			InitialDelaySeconds: int32(config.LivenessProbeInitialDelaySeconds),
			TimeoutSeconds:      int32(config.LivenessProbeTimeoutSeconds),
			PeriodSeconds:       int32(config.LivenessProbePeriodSeconds),
		},
		ImagePullPolicy:   config.ImagePullPolicy,
		ProfilesNamespace: config.ProfilesNamespace,
	}

	// the sync interval does not affect the scale to/from zero feature
	// auto-scaling is does via the HTTP API that acts on the deployment Spec.Replicas
	defaultResync := time.Minute * 5

	namespaceScope := config.DefaultFunctionNamespace
	if config.ClusterRole {
		namespaceScope = ""
	}

	kubeInformerOpt := kubeinformers.WithNamespace(namespaceScope)
	kubeInformerFactory := kubeinformers.NewSharedInformerFactoryWithOptions(kubeClient, defaultResync, kubeInformerOpt)

	faasInformerOpt := informers.WithNamespace(namespaceScope)
	faasInformerFactory := informers.NewSharedInformerFactoryWithOptions(faasClient, defaultResync, faasInformerOpt)

	// this is where we need to swap to the faasInformerFactory
	profileInformerOpt := informers.WithNamespace(config.ProfilesNamespace)
	profileInformerFactory := informers.NewSharedInformerFactoryWithOptions(faasClient, defaultResync, profileInformerOpt)

	profileLister := profileInformerFactory.Openfaas().V1().Profiles().Lister()
	factory := k8s.NewFunctionFactory(kubeClient, deployConfig, profileLister)

	setup := serverSetup{
		config:                 config,
		functionFactory:        factory,
		kubeInformerFactory:    kubeInformerFactory,
		faasInformerFactory:    faasInformerFactory,
		profileInformerFactory: profileInformerFactory,
		kubeClient:             kubeClient,
		faasClient:             faasClient,
	}

	prometheusHost := "prometheus"
	prometheusPort := 9090

	if v, ok := os.LookupEnv("prometheus_host"); ok && len(v) > 0 {
		prometheusHost = v
	}

	if v, ok := os.LookupEnv("prometheus_port"); ok && len(v) > 0 {
		prometheusPort, _ = strconv.Atoi(v)
	}

	query := k8s.NewPrometheusQuery(prometheusHost,
		prometheusPort,
		http.DefaultClient)

	if operator {
		log.Println("Starting operator")
		runOperator(setup, config, query)
	} else {
		log.Println("Starting controller")
		runController(setup, query)
	}
}

type customInformers struct {
	EndpointsInformer  v1core.EndpointsInformer
	DeploymentInformer v1apps.DeploymentInformer
	FunctionsInformer  v1.FunctionInformer
}

func startInformers(setup serverSetup, stopCh <-chan struct{}, operator bool) customInformers {
	kubeInformerFactory := setup.kubeInformerFactory
	faasInformerFactory := setup.faasInformerFactory

	var functions v1.FunctionInformer
	if operator {
		// go faasInformerFactory.Start(stopCh)

		functions = faasInformerFactory.Openfaas().V1().Functions()
		go functions.Informer().Run(stopCh)
		if ok := cache.WaitForNamedCacheSync("faas-netes:functions", stopCh, functions.Informer().HasSynced); !ok {
			log.Fatalf("failed to wait for cache to sync")
		}
	}

	// go kubeInformerFactory.Start(stopCh)

	deployments := kubeInformerFactory.Apps().V1().Deployments()
	go deployments.Informer().Run(stopCh)
	if ok := cache.WaitForNamedCacheSync("faas-netes:deployments", stopCh, deployments.Informer().HasSynced); !ok {
		log.Fatalf("failed to wait for cache to sync")
	}

	endpoints := kubeInformerFactory.Core().V1().Endpoints()
	go endpoints.Informer().Run(stopCh)
	if ok := cache.WaitForNamedCacheSync("faas-netes:endpoints", stopCh, endpoints.Informer().HasSynced); !ok {
		log.Fatalf("failed to wait for cache to sync")
	}

	// go setup.profileInformerFactory.Start(stopCh)

	profileInformerFactory := setup.profileInformerFactory
	profiles := profileInformerFactory.Openfaas().V1().Profiles()
	go profiles.Informer().Run(stopCh)
	if ok := cache.WaitForNamedCacheSync("faas-netes:profiles", stopCh, profiles.Informer().HasSynced); !ok {
		log.Fatalf("failed to wait for cache to sync")
	}

	return customInformers{
		EndpointsInformer:  endpoints,
		DeploymentInformer: deployments,
		FunctionsInformer:  functions,
	}
}

// runController runs the faas-netes imperative controller
func runController(setup serverSetup, query *k8s.PrometheusQuery) {
	config := setup.config
	kubeClient := setup.kubeClient
	factory := setup.functionFactory

	// set up signals so we handle the first shutdown signal gracefully
	stopCh := signals.SetupSignalHandler()
	operator := false
	listers := startInformers(setup, stopCh, operator)

	functionLookup := k8s.NewFunctionLookup(config.DefaultFunctionNamespace, listers.EndpointsInformer.Lister())

	bootstrapHandlers := providertypes.FaaSHandlers{
		FunctionProxy:        proxy.NewHandlerFunc(config.FaaSConfig, functionLookup),
		DeleteHandler:        handlers.MakeDeleteHandler(config.DefaultFunctionNamespace, kubeClient),
		DeployHandler:        handlers.MakeDeployHandler(config.DefaultFunctionNamespace, factory),
		FunctionReader:       handlers.MakeFunctionReader(config.DefaultFunctionNamespace, listers.DeploymentInformer.Lister()),
		ReplicaReader:        handlers.MakeReplicaReader(config.DefaultFunctionNamespace, listers.DeploymentInformer.Lister(), query),
		ReplicaUpdater:       handlers.MakeReplicaUpdater(config.DefaultFunctionNamespace, kubeClient),
		UpdateHandler:        handlers.MakeUpdateHandler(config.DefaultFunctionNamespace, factory),
		HealthHandler:        handlers.MakeHealthHandler(),
		InfoHandler:          handlers.MakeInfoHandler(version.BuildVersion(), version.GitCommit),
		SecretHandler:        handlers.MakeSecretHandler(config.DefaultFunctionNamespace, kubeClient),
		LogHandler:           logs.NewLogHandlerFunc(k8s.NewLogRequestor(kubeClient, config.DefaultFunctionNamespace), config.FaaSConfig.WriteTimeout),
		ListNamespaceHandler: handlers.MakeNamespacesLister(config.DefaultFunctionNamespace, config.ClusterRole, kubeClient),
	}

	faasProvider.Serve(&bootstrapHandlers, &config.FaaSConfig)
}

// runOperator runs the CRD Operator
func runOperator(setup serverSetup, cfg config.BootstrapConfig, query *k8s.PrometheusQuery) {
	kubeClient := setup.kubeClient
	faasClient := setup.faasClient
	kubeInformerFactory := setup.kubeInformerFactory
	faasInformerFactory := setup.faasInformerFactory

	// the operator wraps the FunctionFactory with its own type
	factory := controller.FunctionFactory{
		Factory: setup.functionFactory,
	}

	setupLogging()
	// set up signals so we handle the first shutdown signal gracefully
	stopCh := signals.SetupSignalHandler()
	// set up signals so we handle the first shutdown signal gracefully

	operator := true
	listers := startInformers(setup, stopCh, operator)

	ctrl := controller.NewController(
		kubeClient,
		faasClient,
		kubeInformerFactory,
		faasInformerFactory,
		factory,
	)

	srv := server.New(faasClient, kubeClient, listers.EndpointsInformer, listers.DeploymentInformer.Lister(), cfg.ClusterRole, cfg, query)

	go srv.Start()
	if err := ctrl.Run(1, stopCh); err != nil {
		glog.Fatalf("Error running controller: %s", err.Error())
	}
}

// serverSetup is a container for the config and clients needed to start the
// faas-netes controller or operator
type serverSetup struct {
	config                 config.BootstrapConfig
	kubeClient             *kubernetes.Clientset
	faasClient             *clientset.Clientset
	functionFactory        k8s.FunctionFactory
	kubeInformerFactory    kubeinformers.SharedInformerFactory
	faasInformerFactory    informers.SharedInformerFactory
	profileInformerFactory informers.SharedInformerFactory
}

func setupLogging() {
	klogFlags := flag.NewFlagSet("klog", flag.ExitOnError)
	glog.InitFlags(klogFlags)

	// Sync the glog and klog flags.
	flag.CommandLine.VisitAll(func(f1 *flag.Flag) {
		f2 := klogFlags.Lookup(f1.Name)
		if f2 != nil {
			value := f1.Value.String()
			_ = f2.Value.Set(value)
		}
	})
}
