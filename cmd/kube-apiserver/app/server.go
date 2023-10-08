/*
Copyright 2014 The Kubernetes Authors.

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

// Package app does all of the work necessary to create a Kubernetes
// APIServer by binding together the API, master and APIServer infrastructure.
// It can be configured and called directly or via the hyperkube framework.
package app

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/spf13/cobra"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	extensionsapiserver "k8s.io/apiextensions-apiserver/pkg/apiserver"
	"k8s.io/apimachinery/pkg/runtime"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/admission"
	genericapifilters "k8s.io/apiserver/pkg/endpoints/filters"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/egressselector"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/apiserver/pkg/util/notfoundhandler"
	"k8s.io/apiserver/pkg/util/webhook"
	"k8s.io/client-go/dynamic"
	clientgoinformers "k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/keyutil"
	cloudprovider "k8s.io/cloud-provider"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/cli/globalflag"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	_ "k8s.io/component-base/metrics/prometheus/workqueue"
	"k8s.io/component-base/term"
	"k8s.io/component-base/version"
	"k8s.io/component-base/version/verflag"
	"k8s.io/klog/v2"
	aggregatorapiserver "k8s.io/kube-aggregator/pkg/apiserver"
	aggregatorscheme "k8s.io/kube-aggregator/pkg/apiserver/scheme"
	"k8s.io/kubernetes/pkg/features"

	"k8s.io/kubernetes/cmd/kube-apiserver/app/options"
	"k8s.io/kubernetes/pkg/api/legacyscheme" // 引入legacyscheme，内部的init方法实现资源注册表的注册
	"k8s.io/kubernetes/pkg/capabilities"
	"k8s.io/kubernetes/pkg/controlplane" // 引入master，内部的init方法实现k8s所有资源的注册
	controlplaneapiserver "k8s.io/kubernetes/pkg/controlplane/apiserver"
	"k8s.io/kubernetes/pkg/controlplane/reconcilers"
	generatedopenapi "k8s.io/kubernetes/pkg/generated/openapi"
	kubeapiserveradmission "k8s.io/kubernetes/pkg/kubeapiserver/admission"
	kubeoptions "k8s.io/kubernetes/pkg/kubeapiserver/options"
	"k8s.io/kubernetes/pkg/serviceaccount"
)

// 将默认的特性门配置添加到程序中，以控制程序的一些功能开关状态
func init() {
	utilruntime.Must(logsapi.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
}

// NewAPIServerCommand creates a *cobra.Command object with default parameters
// 返回一个带有默认参数的*cobra.Command对象
func NewAPIServerCommand() *cobra.Command {
	// 创建一个 ServerRunOptions 类型的对象 s，用于存储 API Server 的运行参数和配置
	s := options.NewServerRunOptions()
	// 创建一个 Cobra 命令对象
	cmd := &cobra.Command{
		Use: "kube-apiserver",
		Long: `The Kubernetes API server validates and configures data
for the api objects which include pods, services, replicationcontrollers, and
others. The API Server services REST operations and provides the frontend to the
cluster's shared state through which all other components interact.`,

		// stop printing usage when the command errors
		// 命令发生错误时不显示用法帮助信息
		SilenceUsage: true,
		// 在运行命令之前执行的函数，用于执行一些初始化操作，例如禁用 client-go 库的警告日志。
		PersistentPreRunE: func(*cobra.Command, []string) error {
			// silence client-go warnings.
			// kube-apiserver loopback clients should not log self-issued warnings.
			rest.SetDefaultWarningHandler(rest.NoWarnings{})
			return nil
		},
		// 命令的实际运行逻辑
		RunE: func(cmd *cobra.Command, args []string) error {
			// 检查是否需要打印版本信息并退出。
			verflag.PrintAndExitIfRequested()
			fs := cmd.Flags()

			// Activate logging as soon as possible, after that
			// show flags with the final logging configuration.
			// 激活日志记录，并根据最终的日志配置打印命令行标志。
			if err := logsapi.ValidateAndApply(s.Logs, utilfeature.DefaultFeatureGate); err != nil {
				return err
			}
			cliflag.PrintFlags(fs)

			// set default options
			// 设置默认选项并完成选项的配置。
			completedOptions, err := s.Complete()
			if err != nil {
				return err
			}

			// validate options
			// 验证配置选项，如果存在错误则返回错误。
			if errs := completedOptions.Validate(); len(errs) != 0 {
				return utilerrors.NewAggregate(errs)
			}
			// add feature enablement metrics
			// 添加功能启用度量指标
			utilfeature.DefaultMutableFeatureGate.AddMetrics()
			// 运行 API Server，同时设置信号处理程序
			return Run(completedOptions, genericapiserver.SetupSignalHandler())
		},
		// 定义了命令的参数验证逻辑，确保不接受任何参数。
		Args: func(cmd *cobra.Command, args []string) error {
			for _, arg := range args {
				if len(arg) > 0 {
					return fmt.Errorf("%q does not take any arguments, got %q", cmd.CommandPath(), args)
				}
			}
			return nil
		},
	}

	// 获取命令的标志集合，后续会将各个模块的标志添加到这里。
	fs := cmd.Flags()
	// 用于管理命令的各种标志和选项，包括全局标志和版本标志。
	namedFlagSets := s.Flags()
	verflag.AddFlags(namedFlagSets.FlagSet("global"))
	globalflag.AddGlobalFlags(namedFlagSets.FlagSet("global"), cmd.Name(), logs.SkipLoggingConfigurationFlags())
	options.AddCustomGlobalFlags(namedFlagSets.FlagSet("generic"))
	for _, f := range namedFlagSets.FlagSets {
		fs.AddFlagSet(f)
	}

	// 设置用法和帮助函数，以根据终端窗口的大小动态调整显示。
	cols, _, _ := term.TerminalSize(cmd.OutOrStdout())
	cliflag.SetUsageAndHelpFunc(cmd, namedFlagSets, cols)

	return cmd
}

// Run runs the specified APIServer.  This should never exit.
// 用于运行Kubernetes API Server
func Run(opts options.CompletedOptions, stopCh <-chan struct{}) error {
	// To help debugging, immediately log version
	// 记录信息
	klog.Infof("Version: %+v", version.Get())

	klog.InfoS("Golang settings", "GOGC", os.Getenv("GOGC"), "GOMAXPROCS", os.Getenv("GOMAXPROCS"), "GOTRACEBACK", os.Getenv("GOTRACEBACK"))

	// 创建配置文件
	config, err := NewConfig(opts)
	if err != nil {
		return err
	}
	// 完成配置
	completed, err := config.Complete()
	if err != nil {
		return err
	}
	// 创建服务链
	server, err := CreateServerChain(completed)
	if err != nil {
		return err
	}
	// 准备 API 聚合器（APIAggregator）运行
	prepared, err := server.PrepareRun()
	if err != nil {
		return err
	}
	// 启动 API Server，并将停止信号 (stopCh) 传递给 API Server 实例，以便在需要停止时能够优雅地关闭 API Server
	return prepared.Run(stopCh)
}

// CreateServerChain creates the apiservers connected via delegation.
func CreateServerChain(config CompletedConfig) (*aggregatorapiserver.APIAggregator, error) {
	// 用于处理请求中的未知路径（404 Not Found），根据请求的路径返回适当的错误响应。
	notFoundHandler := notfoundhandler.New(config.ControlPlane.GenericConfig.Serializer, genericapifilters.NoMuxAndDiscoveryIncompleteKey)
	// 创建了一个用于处理 CustomResourceDefinition（CRD）和其他扩展资源的 API Server apiExtensionsServer
	apiExtensionsServer, err := config.ApiExtensions.New(genericapiserver.NewEmptyDelegateWithCustomHandler(notFoundHandler))
	if err != nil {
		return nil, err
	}
	// 检查是否启用了 CRD API
	crdAPIEnabled := config.ApiExtensions.GenericConfig.MergedResourceConfig.ResourceEnabled(apiextensionsv1.SchemeGroupVersion.WithResource("customresourcedefinitions"))
	// 创建了一个用于处理内置资源的 Kubernetes API Server kubeAPIServer
	kubeAPIServer, err := config.ControlPlane.New(apiExtensionsServer.GenericAPIServer)
	if err != nil {
		return nil, err
	}

	// aggregator comes last in the chain
	// 创建了一个聚合 API Server aggregatorServer，它位于 API Server 链的最后。它负责处理聚合请求，将请求路由到相应的组件上，并提供整体的聚合功能。
	aggregatorServer, err := createAggregatorServer(config.Aggregator, kubeAPIServer.GenericAPIServer, apiExtensionsServer.Informers, crdAPIEnabled)
	if err != nil {
		// we don't need special handling for innerStopCh because the aggregator server doesn't create any go routines
		return nil, err
	}

	return aggregatorServer, nil
}

// CreateProxyTransport creates the dialer infrastructure to connect to the nodes.
func CreateProxyTransport() *http.Transport {
	var proxyDialerFn utilnet.DialFunc
	// Proxying to pods and services is IP-based... don't expect to be able to verify the hostname
	proxyTLSClientConfig := &tls.Config{InsecureSkipVerify: true}
	proxyTransport := utilnet.SetTransportDefaults(&http.Transport{
		DialContext:     proxyDialerFn,
		TLSClientConfig: proxyTLSClientConfig,
	})
	return proxyTransport
}

// CreateKubeAPIServerConfig creates all the resources for running the API server, but runs none of them
func CreateKubeAPIServerConfig(opts options.CompletedOptions) (
	*controlplane.Config,
	aggregatorapiserver.ServiceResolver,
	[]admission.PluginInitializer,
	error,
) {
	proxyTransport := CreateProxyTransport()

	genericConfig, versionedInformers, storageFactory, err := controlplaneapiserver.BuildGenericConfig(
		opts.CompletedOptions,
		[]*runtime.Scheme{legacyscheme.Scheme, extensionsapiserver.Scheme, aggregatorscheme.Scheme},
		generatedopenapi.GetOpenAPIDefinitions,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	capabilities.Setup(opts.AllowPrivileged, opts.MaxConnectionBytesPerSec)

	opts.Metrics.Apply()
	serviceaccount.RegisterMetrics()

	config := &controlplane.Config{
		GenericConfig: genericConfig,
		ExtraConfig: controlplane.ExtraConfig{
			APIResourceConfigSource: storageFactory.APIResourceConfigSource,
			StorageFactory:          storageFactory,
			EventTTL:                opts.EventTTL,
			KubeletClientConfig:     opts.KubeletConfig,
			EnableLogsSupport:       opts.EnableLogsHandler,
			ProxyTransport:          proxyTransport,

			ServiceIPRange:          opts.PrimaryServiceClusterIPRange,
			APIServerServiceIP:      opts.APIServerServiceIP,
			SecondaryServiceIPRange: opts.SecondaryServiceClusterIPRange,

			APIServerServicePort: 443,

			ServiceNodePortRange:      opts.ServiceNodePortRange,
			KubernetesServiceNodePort: opts.KubernetesServiceNodePort,

			EndpointReconcilerType: reconcilers.Type(opts.EndpointReconcilerType),
			MasterCount:            opts.MasterCount,

			ServiceAccountIssuer:        opts.ServiceAccountIssuer,
			ServiceAccountMaxExpiration: opts.ServiceAccountTokenMaxExpiration,
			ExtendExpiration:            opts.Authentication.ServiceAccounts.ExtendExpiration,

			VersionedInformers: versionedInformers,
		},
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.UnknownVersionInteroperabilityProxy) {
		config.ExtraConfig.PeerEndpointLeaseReconciler, err = controlplaneapiserver.CreatePeerEndpointLeaseReconciler(*genericConfig, storageFactory)
		if err != nil {
			return nil, nil, nil, err
		}
		// build peer proxy config only if peer ca file exists
		if opts.PeerCAFile != "" {
			config.ExtraConfig.PeerProxy, err = controlplaneapiserver.BuildPeerProxy(versionedInformers, genericConfig.StorageVersionManager, opts.ProxyClientCertFile,
				opts.ProxyClientKeyFile, opts.PeerCAFile, opts.PeerAdvertiseAddress, genericConfig.APIServerID, config.ExtraConfig.PeerEndpointLeaseReconciler, config.GenericConfig.Serializer)
			if err != nil {
				return nil, nil, nil, err
			}
		}
	}

	clientCAProvider, err := opts.Authentication.ClientCert.GetClientCAContentProvider()
	if err != nil {
		return nil, nil, nil, err
	}
	config.ExtraConfig.ClusterAuthenticationInfo.ClientCA = clientCAProvider

	requestHeaderConfig, err := opts.Authentication.RequestHeader.ToAuthenticationRequestHeaderConfig()
	if err != nil {
		return nil, nil, nil, err
	}
	if requestHeaderConfig != nil {
		config.ExtraConfig.ClusterAuthenticationInfo.RequestHeaderCA = requestHeaderConfig.CAContentProvider
		config.ExtraConfig.ClusterAuthenticationInfo.RequestHeaderAllowedNames = requestHeaderConfig.AllowedClientNames
		config.ExtraConfig.ClusterAuthenticationInfo.RequestHeaderExtraHeaderPrefixes = requestHeaderConfig.ExtraHeaderPrefixes
		config.ExtraConfig.ClusterAuthenticationInfo.RequestHeaderGroupHeaders = requestHeaderConfig.GroupHeaders
		config.ExtraConfig.ClusterAuthenticationInfo.RequestHeaderUsernameHeaders = requestHeaderConfig.UsernameHeaders
	}

	err = validateCloudProviderOptions(opts.CloudProvider)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to validate cloud provider: %w", err)
	}

	// setup admission
	admissionConfig := &kubeapiserveradmission.Config{
		ExternalInformers:    versionedInformers,
		LoopbackClientConfig: genericConfig.LoopbackClientConfig,
		CloudConfigFile:      opts.CloudProvider.CloudConfigFile,
	}
	serviceResolver := buildServiceResolver(opts.EnableAggregatorRouting, genericConfig.LoopbackClientConfig.Host, versionedInformers)
	pluginInitializers, admissionPostStartHook, err := admissionConfig.New(proxyTransport, genericConfig.EgressSelector, serviceResolver, genericConfig.TracerProvider)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create admission plugin initializer: %v", err)
	}
	clientgoExternalClient, err := clientset.NewForConfig(genericConfig.LoopbackClientConfig)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create real client-go external client: %w", err)
	}
	dynamicExternalClient, err := dynamic.NewForConfig(genericConfig.LoopbackClientConfig)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create real dynamic external client: %w", err)
	}
	err = opts.Admission.ApplyTo(
		genericConfig,
		versionedInformers,
		clientgoExternalClient,
		dynamicExternalClient,
		utilfeature.DefaultFeatureGate,
		pluginInitializers...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to apply admission: %w", err)
	}
	if err := config.GenericConfig.AddPostStartHook("start-kube-apiserver-admission-initializer", admissionPostStartHook); err != nil {
		return nil, nil, nil, err
	}

	if config.GenericConfig.EgressSelector != nil {
		// Use the config.GenericConfig.EgressSelector lookup to find the dialer to connect to the kubelet
		config.ExtraConfig.KubeletClientConfig.Lookup = config.GenericConfig.EgressSelector.Lookup

		// Use the config.GenericConfig.EgressSelector lookup as the transport used by the "proxy" subresources.
		networkContext := egressselector.Cluster.AsNetworkContext()
		dialer, err := config.GenericConfig.EgressSelector.Lookup(networkContext)
		if err != nil {
			return nil, nil, nil, err
		}
		c := proxyTransport.Clone()
		c.DialContext = dialer
		config.ExtraConfig.ProxyTransport = c
	}

	// Load and set the public keys.
	var pubKeys []interface{}
	for _, f := range opts.Authentication.ServiceAccounts.KeyFiles {
		keys, err := keyutil.PublicKeysFromFile(f)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to parse key file %q: %v", f, err)
		}
		pubKeys = append(pubKeys, keys...)
	}
	config.ExtraConfig.ServiceAccountIssuerURL = opts.Authentication.ServiceAccounts.Issuers[0]
	config.ExtraConfig.ServiceAccountJWKSURI = opts.Authentication.ServiceAccounts.JWKSURI
	config.ExtraConfig.ServiceAccountPublicKeys = pubKeys

	return config, serviceResolver, pluginInitializers, nil
}

func validateCloudProviderOptions(opts *kubeoptions.CloudProviderOptions) error {
	if opts.CloudProvider == "" {
		return nil
	}
	if opts.CloudProvider == "external" {
		if !utilfeature.DefaultFeatureGate.Enabled(features.DisableCloudProviders) {
			return fmt.Errorf("when using --cloud-provider set to '%s', "+
				"please set DisableCloudProviders feature to true", opts.CloudProvider)
		}
		if !utilfeature.DefaultFeatureGate.Enabled(features.DisableKubeletCloudCredentialProviders) {
			return fmt.Errorf("when using --cloud-provider set to '%s', "+
				"please set DisableKubeletCloudCredentialProviders feature to true", opts.CloudProvider)
		}
		return nil
	} else if cloudprovider.IsDeprecatedInternal(opts.CloudProvider) {
		if utilfeature.DefaultFeatureGate.Enabled(features.DisableCloudProviders) {
			return fmt.Errorf("when using --cloud-provider set to '%s', "+
				"please set DisableCloudProviders feature to false", opts.CloudProvider)
		}
		if utilfeature.DefaultFeatureGate.Enabled(features.DisableKubeletCloudCredentialProviders) {
			return fmt.Errorf("when using --cloud-provider set to '%s', "+
				"please set DisableKubeletCloudCredentialProviders feature to false", opts.CloudProvider)
		}
		return nil
	}
	return fmt.Errorf("unknown --cloud-provider : %s", opts.CloudProvider)
}

var testServiceResolver webhook.ServiceResolver

// SetServiceResolverForTests allows the service resolver to be overridden during tests.
// Tests using this function must run serially as this function is not safe to call concurrently with server start.
func SetServiceResolverForTests(resolver webhook.ServiceResolver) func() {
	if testServiceResolver != nil {
		panic("test service resolver is set: tests are either running concurrently or clean up was skipped")
	}

	testServiceResolver = resolver

	return func() {
		testServiceResolver = nil
	}
}

func buildServiceResolver(enabledAggregatorRouting bool, hostname string, informer clientgoinformers.SharedInformerFactory) webhook.ServiceResolver {
	if testServiceResolver != nil {
		return testServiceResolver
	}

	var serviceResolver webhook.ServiceResolver
	if enabledAggregatorRouting {
		serviceResolver = aggregatorapiserver.NewEndpointServiceResolver(
			informer.Core().V1().Services().Lister(),
			informer.Core().V1().Endpoints().Lister(),
		)
	} else {
		serviceResolver = aggregatorapiserver.NewClusterIPServiceResolver(
			informer.Core().V1().Services().Lister(),
		)
	}

	// resolve kubernetes.default.svc locally
	if localHost, err := url.Parse(hostname); err == nil {
		serviceResolver = aggregatorapiserver.NewLoopbackServiceResolver(serviceResolver, localHost)
	}
	return serviceResolver
}
