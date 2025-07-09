package gateway

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/miekg/dns"
	core "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	externaldnsv1 "sigs.k8s.io/external-dns/apis/v1alpha1"
	"sigs.k8s.io/external-dns/source"
	gatewayapi_v1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayapi_v1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	gatewayClient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

const (
	defaultResyncPeriod              = 0
	ingressHostnameIndex             = "ingressHostname"
	serviceHostnameIndex             = "serviceHostname"
	gatewayUniqueIndex               = "gatewayIndex"
	httpRouteHostnameIndex           = "httpRouteHostname"
	tlsRouteHostnameIndex            = "tlsRouteHostname"
	grpcRouteHostnameIndex           = "grpcRouteHostname"
	externalDNSHostnameIndex         = "externalDNSHostname"
	hostnameAnnotationKey            = "coredns.io/hostname"
	externalDnsHostnameAnnotationKey = "external-dns.alpha.kubernetes.io/hostname"
	externalDNSEndpointGroup         = "externaldns.k8s.io/v1alpha1"
	externalDNSEndpointKind          = "DNSEndpoint"
)

var (
	apiextensionsClient  *apiextensionsclientset.Clientset
	externaldnsCRDClient rest.Interface
)

// KubeController stores the current runtime configuration and cache
type KubeController struct {
	client      kubernetes.Interface
	gwClient    gatewayClient.Interface
	controllers []cache.SharedIndexInformer
	hasSynced   bool
}

func newKubeController(ctx context.Context, c *kubernetes.Clientset, gw *gatewayClient.Clientset, originalGateway *Gateway) *KubeController {
	log.Infof("Building k8s_gateway controller")

	ctrl := &KubeController{
		client:   c,
		gwClient: gw,
	}

	configuredResources := dereferenceStrings(originalGateway.ConfiguredResources)
	routingResources := []string{"HTTPRoute", "TLSRoute", "GRPCRoute"}

	shouldInitGateway := false
	for _, r := range routingResources {
		if slices.Contains(configuredResources, r) {
			shouldInitGateway = true
			break
		}
	}

	if crdExists(apiextensionsClient, "gatewayclasses.gateway.networking.k8s.io") && shouldInitGateway {
		gatewayController := cache.NewSharedIndexInformer(
			&cache.ListWatch{
				ListFunc:  gatewayLister(ctx, ctrl.gwClient, core.NamespaceAll),
				WatchFunc: gatewayWatcher(ctx, ctrl.gwClient, core.NamespaceAll),
			},
			&gatewayapi_v1.Gateway{},
			defaultResyncPeriod,
			cache.Indexers{gatewayUniqueIndex: gatewayIndexFunc},
		)
		ctrl.controllers = append(ctrl.controllers, gatewayController)
		log.Infof("GatewayAPI controller initialized")

		for _, resourceName := range routingResources {
			if !slices.Contains(configuredResources, resourceName) {
				continue
			}
			resource := originalGateway.lookupResource(resourceName)
			if resource == nil {
				continue
			}

			switch resourceName {
			case "HTTPRoute":
				httpRouteController := cache.NewSharedIndexInformer(
					&cache.ListWatch{
						ListFunc:  httpRouteLister(ctx, ctrl.gwClient, core.NamespaceAll),
						WatchFunc: httpRouteWatcher(ctx, ctrl.gwClient, core.NamespaceAll),
					},
					&gatewayapi_v1.HTTPRoute{},
					defaultResyncPeriod,
					cache.Indexers{httpRouteHostnameIndex: httpRouteHostnameIndexFunc},
				)
				resource.lookup = lookupHttpRouteIndex(httpRouteController, gatewayController, originalGateway.resourceFilters.gatewayClasses)
				ctrl.controllers = append(ctrl.controllers, httpRouteController)
				log.Infof("HTTPRoute controller initialized")

			case "TLSRoute":
				tlsRouteController := cache.NewSharedIndexInformer(
					&cache.ListWatch{
						ListFunc:  tlsRouteLister(ctx, ctrl.gwClient, core.NamespaceAll),
						WatchFunc: tlsRouteWatcher(ctx, ctrl.gwClient, core.NamespaceAll),
					},
					&gatewayapi_v1alpha2.TLSRoute{},
					defaultResyncPeriod,
					cache.Indexers{tlsRouteHostnameIndex: tlsRouteHostnameIndexFunc},
				)
				resource.lookup = lookupTLSRouteIndex(tlsRouteController, gatewayController, originalGateway.resourceFilters.gatewayClasses)
				ctrl.controllers = append(ctrl.controllers, tlsRouteController)
				log.Infof("TLSRoute controller initialized")

			case "GRPCRoute":
				grpcRouteController := cache.NewSharedIndexInformer(
					&cache.ListWatch{
						ListFunc:  grpcRouteLister(ctx, ctrl.gwClient, core.NamespaceAll),
						WatchFunc: grpcRouteWatcher(ctx, ctrl.gwClient, core.NamespaceAll),
					},
					&gatewayapi_v1.GRPCRoute{},
					defaultResyncPeriod,
					cache.Indexers{grpcRouteHostnameIndex: grpcRouteHostnameIndexFunc},
				)
				resource.lookup = lookupGRPCRouteIndex(grpcRouteController, gatewayController, originalGateway.resourceFilters.gatewayClasses)
				ctrl.controllers = append(ctrl.controllers, grpcRouteController)
				log.Infof("GRPCRoute controller initialized")
			}
		}
	}

	for _, resourceName := range []string{"Ingress", "Service"} {
		if slices.Contains(dereferenceStrings(originalGateway.ConfiguredResources), resourceName) {
			if resource := originalGateway.lookupResource(resourceName); resource != nil {
				switch resourceName {
				case "Ingress":
					ingressController := cache.NewSharedIndexInformer(
						&cache.ListWatch{
							ListFunc:  ingressLister(ctx, ctrl.client, core.NamespaceAll),
							WatchFunc: ingressWatcher(ctx, ctrl.client, core.NamespaceAll),
						},
						&networking.Ingress{},
						defaultResyncPeriod,
						cache.Indexers{ingressHostnameIndex: ingressHostnameIndexFunc},
					)
					resource.lookup = lookupIngressIndex(ingressController, originalGateway.resourceFilters.ingressClasses)
					ctrl.controllers = append(ctrl.controllers, ingressController)
					log.Infof("Ingress controller initialized")

				case "Service":
					serviceController := cache.NewSharedIndexInformer(
						&cache.ListWatch{
							ListFunc:  serviceLister(ctx, ctrl.client, core.NamespaceAll),
							WatchFunc: serviceWatcher(ctx, ctrl.client, core.NamespaceAll),
						},
						&core.Service{},
						defaultResyncPeriod,
						cache.Indexers{serviceHostnameIndex: serviceHostnameIndexFunc},
					)
					resource.lookup = lookupServiceIndex(serviceController)
					ctrl.controllers = append(ctrl.controllers, serviceController)
					log.Infof("Service controller initialized")
				}
			}
		}
	}

	if crdExists(apiextensionsClient, "dnsendpoints.externaldns.k8s.io") && slices.Contains(dereferenceStrings(originalGateway.ConfiguredResources), "DNSEndpoint") {
		if resource := originalGateway.lookupResource("DNSEndpoint"); resource != nil {
			dnsEndpointController := cache.NewSharedIndexInformer(
				&cache.ListWatch{
					WatchFunc: dnsEndpointWatcher(ctx, core.NamespaceAll),
					ListFunc:  dnsEndpointLister(ctx, core.NamespaceAll),
				},
				&externaldnsv1.DNSEndpoint{},
				defaultResyncPeriod,
				cache.Indexers{externalDNSHostnameIndex: dnsEndpointTargetIndexFunc},
			)
			resource.lookup = lookupDNSEndpoint(dnsEndpointController)
			ctrl.controllers = append(ctrl.controllers, dnsEndpointController)
			log.Infof("DNSEndpoint controller initialized")
		}
	}

	return ctrl
}

func (ctrl *KubeController) run() {
	stopCh := make(chan struct{})
	defer close(stopCh)

	var synced []cache.InformerSynced

	log.Infof("Starting k8s_gateway controller")
	for _, ctrl := range ctrl.controllers {
		go ctrl.Run(stopCh)
		synced = append(synced, ctrl.HasSynced)
	}

	log.Infof("Waiting for controllers to sync")
	if !cache.WaitForCacheSync(stopCh, synced...) {
		ctrl.hasSynced = false
	}
	log.Infof("Synced all required resources")
	ctrl.hasSynced = true

	<-stopCh
}

// HasSynced returns true if all controllers have been synced
func (ctrl *KubeController) HasSynced() bool {
	return ctrl.hasSynced
}

// RunKubeController kicks off the k8s controllers
func (gw *Gateway) RunKubeController(ctx context.Context) error {
	config, err := gw.getClientConfig()
	if err != nil {
		return err
	}

	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	apiextensionsClient, err = apiextensionsclientset.NewForConfig(config)
	if err != nil {
		return err
	}

	gwAPIClient, err := gatewayClient.NewForConfig(config)
	if err != nil {
		return err
	}

	externaldnsCRDClient, _, err = source.NewCRDClientForAPIVersionKind(kubeClient, gw.configFile, "", externalDNSEndpointGroup, externalDNSEndpointKind)
	if err != nil {
		log.Warningf("crd %s not found. ignoring and continuing execution", externalDNSEndpointGroup)
	}

	gw.Controller = newKubeController(ctx, kubeClient, gwAPIClient, gw)
	go gw.Controller.run()

	return nil
}

func crdExists(clientset *apiextensionsclientset.Clientset, crdName string) bool {
	_, err := clientset.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), crdName, metav1.GetOptions{})
	if err != nil {
		log.Warningf("error getting crd %s, error: %s", crdName, err.Error())
	} else {
		log.Infof("crd %s found", crdName)
	}
	return err == nil
}

func (gw *Gateway) getClientConfig() (*rest.Config, error) {
	if gw.configFile != "" {
		overrides := &clientcmd.ConfigOverrides{}
		overrides.CurrentContext = gw.configContext

		config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: gw.configFile},
			overrides,
		)

		return config.ClientConfig()
	}

	return rest.InClusterConfig()
}

func dereferenceStrings(ptrs []*string) []string {
	var strs []string
	for _, ptr := range ptrs {
		if ptr != nil {
			strs = append(strs, *ptr)
		}
	}
	return strs
}

func httpRouteLister(ctx context.Context, c gatewayClient.Interface, ns string) func(metav1.ListOptions) (runtime.Object, error) {
	return func(opts metav1.ListOptions) (runtime.Object, error) {
		return c.GatewayV1().HTTPRoutes(ns).List(ctx, opts)
	}
}

func tlsRouteLister(ctx context.Context, c gatewayClient.Interface, ns string) func(metav1.ListOptions) (runtime.Object, error) {
	return func(opts metav1.ListOptions) (runtime.Object, error) {
		return c.GatewayV1alpha2().TLSRoutes(ns).List(ctx, opts)
	}
}

func grpcRouteLister(ctx context.Context, c gatewayClient.Interface, ns string) func(metav1.ListOptions) (runtime.Object, error) {
	return func(opts metav1.ListOptions) (runtime.Object, error) {
		return c.GatewayV1().GRPCRoutes(ns).List(ctx, opts)
	}
}

func gatewayLister(ctx context.Context, c gatewayClient.Interface, ns string) func(metav1.ListOptions) (runtime.Object, error) {
	return func(opts metav1.ListOptions) (runtime.Object, error) {
		return c.GatewayV1().Gateways(ns).List(ctx, opts)
	}
}

func ingressLister(ctx context.Context, c kubernetes.Interface, ns string) func(metav1.ListOptions) (runtime.Object, error) {
	return func(opts metav1.ListOptions) (runtime.Object, error) {
		return c.NetworkingV1().Ingresses(ns).List(ctx, opts)
	}
}

func serviceLister(ctx context.Context, c kubernetes.Interface, ns string) func(metav1.ListOptions) (runtime.Object, error) {
	return func(opts metav1.ListOptions) (runtime.Object, error) {
		return c.CoreV1().Services(ns).List(ctx, opts)
	}
}

func httpRouteWatcher(ctx context.Context, c gatewayClient.Interface, ns string) func(metav1.ListOptions) (watch.Interface, error) {
	return func(opts metav1.ListOptions) (watch.Interface, error) {
		return c.GatewayV1().HTTPRoutes(ns).Watch(ctx, opts)
	}
}

func tlsRouteWatcher(ctx context.Context, c gatewayClient.Interface, ns string) func(metav1.ListOptions) (watch.Interface, error) {
	return func(opts metav1.ListOptions) (watch.Interface, error) {
		return c.GatewayV1alpha2().TLSRoutes(ns).Watch(ctx, opts)
	}
}

func grpcRouteWatcher(ctx context.Context, c gatewayClient.Interface, ns string) func(metav1.ListOptions) (watch.Interface, error) {
	return func(opts metav1.ListOptions) (watch.Interface, error) {
		return c.GatewayV1().GRPCRoutes(ns).Watch(ctx, opts)
	}
}

func gatewayWatcher(ctx context.Context, c gatewayClient.Interface, ns string) func(metav1.ListOptions) (watch.Interface, error) {
	return func(opts metav1.ListOptions) (watch.Interface, error) {
		return c.GatewayV1().Gateways(ns).Watch(ctx, opts)
	}
}

func ingressWatcher(ctx context.Context, c kubernetes.Interface, ns string) func(metav1.ListOptions) (watch.Interface, error) {
	return func(opts metav1.ListOptions) (watch.Interface, error) {
		return c.NetworkingV1().Ingresses(ns).Watch(ctx, opts)
	}
}

func serviceWatcher(ctx context.Context, c kubernetes.Interface, ns string) func(metav1.ListOptions) (watch.Interface, error) {
	return func(opts metav1.ListOptions) (watch.Interface, error) {
		return c.CoreV1().Services(ns).Watch(ctx, opts)
	}
}

func dnsEndpointWatcher(ctx context.Context, ns string) func(metav1.ListOptions) (watch.Interface, error) {
	return func(opts metav1.ListOptions) (watch.Interface, error) {
		opts.Watch = true
		return externaldnsCRDClient.Get().
			Resource("dnsendpoints").
			Namespace(ns).
			VersionedParams(&opts, metav1.ParameterCodec).
			Watch(ctx)
	}
}

func dnsEndpointLister(ctx context.Context, ns string) func(metav1.ListOptions) (runtime.Object, error) {
	return func(opts metav1.ListOptions) (runtime.Object, error) {
		return externaldnsCRDClient.Get().
			Resource("dnsendpoints").
			Namespace(ns).
			VersionedParams(&opts, metav1.ParameterCodec).
			Do(ctx).
			Get()
	}
}

// indexes based on "namespace/name" as the key
func gatewayIndexFunc(obj interface{}) ([]string, error) {
	metaObj, err := meta.Accessor(obj)
	if err != nil {
		return []string{""}, fmt.Errorf("object has no meta: %v", err)
	}
	return []string{fmt.Sprintf("%s/%s", metaObj.GetNamespace(), metaObj.GetName())}, nil
}

func httpRouteHostnameIndexFunc(obj interface{}) ([]string, error) {
	httpRoute, ok := obj.(*gatewayapi_v1.HTTPRoute)
	if !ok {
		return []string{}, nil
	}

	var hostnames []string
	for _, hostname := range httpRoute.Spec.Hostnames {
		log.Debugf("Adding index %s for httpRoute %s", httpRoute.Name, hostname)
		hostnames = append(hostnames, string(hostname))
	}
	return hostnames, nil
}

func tlsRouteHostnameIndexFunc(obj interface{}) ([]string, error) {
	tlsRoute, ok := obj.(*gatewayapi_v1alpha2.TLSRoute)
	if !ok {
		return []string{}, nil
	}

	var hostnames []string
	for _, hostname := range tlsRoute.Spec.Hostnames {
		log.Debugf("Adding index %s for tlsRoute %s", tlsRoute.Name, hostname)
		hostnames = append(hostnames, string(hostname))
	}
	return hostnames, nil
}

func grpcRouteHostnameIndexFunc(obj interface{}) ([]string, error) {
	grpcRoute, ok := obj.(*gatewayapi_v1.GRPCRoute)
	if !ok {
		return []string{}, nil
	}

	var hostnames []string
	for _, hostname := range grpcRoute.Spec.Hostnames {
		log.Debugf("Adding index %s for grpcRoute %s", grpcRoute.Name, hostname)
		hostnames = append(hostnames, string(hostname))
	}
	return hostnames, nil
}

func ingressHostnameIndexFunc(obj interface{}) ([]string, error) {
	ingress, ok := obj.(*networking.Ingress)
	if !ok {
		return []string{}, nil
	}

	var hostnames []string
	for _, rule := range ingress.Spec.Rules {
		log.Debugf("Adding index %s for ingress %s", rule.Host, ingress.Name)
		hostnames = append(hostnames, rule.Host)
	}
	return hostnames, nil
}

func serviceHostnameIndexFunc(obj interface{}) ([]string, error) {
	service, ok := obj.(*core.Service)
	if !ok {
		return []string{}, nil
	}

	if service.Spec.Type != core.ServiceTypeLoadBalancer {
		return []string{}, nil
	}

	hostname := service.Name + "." + service.Namespace
	hostnames := []string{}
	if annotation, exists := checkServiceAnnotation(hostnameAnnotationKey, service); exists {
		if checkDomainValid(annotation) {
			hostnames = []string{annotation}
			log.Debugf("Adding index %s for service %s", annotation, service.Name)
		}
	} else if annotation, exists := checkServiceAnnotation(externalDnsHostnameAnnotationKey, service); exists {
		for _, hostname := range splitHostnameAnnotation(annotation) {
			if checkDomainValid(hostname) {
				hostnames = append(hostnames, hostname)
				log.Debugf("Adding index %s for service %s", hostname, service.Name)
			}
		}
	} else {
		hostnames = []string{hostname}
	}

	return hostnames, nil
}

func splitHostnameAnnotation(annotation string) []string {
	return strings.Split(strings.ReplaceAll(annotation, " ", ""), ",")
}

func dnsEndpointTargetIndexFunc(obj interface{}) ([]string, error) {
	dnsEndpoint, ok := obj.(*externaldnsv1.DNSEndpoint)
	if !ok {
		return []string{}, nil
	}
	var hostnames []string
	for _, endpoint := range dnsEndpoint.Spec.Endpoints {
		log.Debugf("Adding index %s for DNSEndpoint %s", endpoint.DNSName, dnsEndpoint.Name)
		hostnames = append(hostnames, endpoint.DNSName)
	}
	return hostnames, nil
}

func checkServiceAnnotation(annotation string, service *core.Service) (string, bool) {
	if annotationValue, exists := service.Annotations[annotation]; exists {
		return strings.ToLower(annotationValue), true
	}

	return "", false
}

func checkDomainValid(domain string) bool {
	if _, ok := dns.IsDomainName(domain); ok {
		// checking RFC 1123 conformance (same as metadata labels)
		if valid := isdns1123Hostname(domain); valid {
			return true
		}
		log.Infof("RFC 1123 conformance failed for FQDN: %s", domain)
	} else {
		log.Infof("Invalid FQDN length: %s", domain)
	}
	return false
}

func lookupServiceIndex(ctrl cache.SharedIndexInformer) func([]string) map[string][]string {
	return func(indexKeys []string) (result map[string][]string) {
		if result == nil {
			result = make(map[string][]string, 0)
		}

		var objs []interface{}
		for _, key := range indexKeys {
			obj, _ := ctrl.GetIndexer().ByIndex(serviceHostnameIndex, strings.ToLower(key))
			objs = append(objs, obj...)
		}
		log.Debugf("Found %d matching Service objects", len(objs))
		for _, obj := range objs {
			service, _ := obj.(*core.Service)

			if len(service.Spec.ExternalIPs) > 0 {
				for _, ip := range service.Spec.ExternalIPs {
					addr, err := netip.ParseAddr(ip)
					if err != nil {
						continue
					}

					if addr.Is4() {
						if result["A"] == nil {
							result["A"] = make([]string, 0)
						}
						result["A"] = append(result["A"], addr.String())
					}
					if addr.Is6() {
						if result["AAAA"] == nil {
							result["AAAA"] = make([]string, 0)
						}
						result["AAAA"] = append(result["AAAA"], addr.String())
					}
				}
				// in case externalIPs are defined, ignoring status field completely
				return
			}

			var fetchedResults = fetchServiceLoadBalancerIPs(service.Status.LoadBalancer.Ingress)
			result = appenddnsResults(result, fetchedResults)
		}
		return
	}
}

func lookupHttpRouteIndex(http, gw cache.SharedIndexInformer, gwclasses []string) func([]string) map[string][]string {
	return func(indexKeys []string) (result map[string][]string) {
		var objs []interface{}
		for _, key := range indexKeys {
			obj, _ := http.GetIndexer().ByIndex(httpRouteHostnameIndex, strings.ToLower(key))
			objs = append(objs, obj...)
		}
		log.Debugf("Found %d matching httpRoute objects", len(objs))

		for _, obj := range objs {
			httpRoute, _ := obj.(*gatewayapi_v1.HTTPRoute)

			var fetchedResults = lookupGateways(gw, httpRoute.Spec.ParentRefs, httpRoute.Namespace, gwclasses)
			result = appenddnsResults(result, fetchedResults)
		}
		return
	}
}

func lookupTLSRouteIndex(tls, gw cache.SharedIndexInformer, gwclasses []string) func([]string) map[string][]string {
	return func(indexKeys []string) (result map[string][]string) {
		var objs []interface{}
		for _, key := range indexKeys {
			obj, _ := tls.GetIndexer().ByIndex(tlsRouteHostnameIndex, strings.ToLower(key))
			objs = append(objs, obj...)
		}
		log.Debugf("Found %d matching tlsRoute objects", len(objs))

		for _, obj := range objs {
			tlsRoute, _ := obj.(*gatewayapi_v1alpha2.TLSRoute)

			var fetchedResults = lookupGateways(gw, tlsRoute.Spec.ParentRefs, tlsRoute.Namespace, gwclasses)
			result = appenddnsResults(result, fetchedResults)
		}
		return
	}
}

func lookupGRPCRouteIndex(grpc, gw cache.SharedIndexInformer, gwclasses []string) func([]string) map[string][]string {
	return func(indexKeys []string) (result map[string][]string) {
		var objs []interface{}
		for _, key := range indexKeys {
			obj, _ := grpc.GetIndexer().ByIndex(grpcRouteHostnameIndex, strings.ToLower(key))
			objs = append(objs, obj...)
		}
		log.Debugf("Found %d matching grpcRoute objects", len(objs))

		for _, obj := range objs {
			grpcRoute, _ := obj.(*gatewayapi_v1.GRPCRoute)

			var fetchedResults = lookupGateways(gw, grpcRoute.Spec.ParentRefs, grpcRoute.Namespace, gwclasses)
			result = appenddnsResults(result, fetchedResults)
		}
		return
	}
}

func lookupGateways(gw cache.SharedIndexInformer, refs []gatewayapi_v1.ParentReference, ns string, gwclasses []string) (result map[string][]string) {
	for _, gwRef := range refs {

		if gwRef.Namespace != nil {
			ns = string(*gwRef.Namespace)
		}
		gwKey := fmt.Sprintf("%s/%s", ns, gwRef.Name)

		gwObjs, _ := gw.GetIndexer().ByIndex(gatewayUniqueIndex, gwKey)
		log.Debugf("Found %d matching gateway objects", len(gwObjs))

		for _, gwObj := range gwObjs {
			gw, _ := gwObj.(*gatewayapi_v1.Gateway)

			if len(gwclasses) > 0 && !slices.Contains(gwclasses, string(gw.Spec.GatewayClassName)) {
				log.Debugf("Skipping gateway of '%s' gatewayClass", string(gw.Spec.GatewayClassName))
				continue
			}

			var fetchedResults = fetchGatewayIPs(gw)
			result = appenddnsResults(result, fetchedResults)
		}
	}
	return
}

func lookupIngressIndex(ctrl cache.SharedIndexInformer, ingclasses []string) func([]string) map[string][]string {
	return func(indexKeys []string) (result map[string][]string) {
		var objs []interface{}
		for _, key := range indexKeys {
			obj, _ := ctrl.GetIndexer().ByIndex(ingressHostnameIndex, strings.ToLower(key))
			objs = append(objs, obj...)
		}
		log.Debugf("Found %d matching Ingress objects", len(objs))
		for _, obj := range objs {
			ingress, _ := obj.(*networking.Ingress)

			if len(ingclasses) > 0 && !slices.Contains(ingclasses, *ingress.Spec.IngressClassName) {
				log.Debugf("Skipping ingress of '%s' ingressClass", *ingress.Spec.IngressClassName)
				continue
			}

			var fetchedResults = fetchIngressLoadBalancerIPs(ingress.Status.LoadBalancer.Ingress)
			result = appenddnsResults(result, fetchedResults)
		}

		return
	}
}

func lookupDNSEndpoint(ctrl cache.SharedIndexInformer) func([]string) (results map[string][]string) {
	return func(indexKeys []string) (result map[string][]string) {
		if result == nil {
			result = make(map[string][]string, 0)
		}

		var objs []interface{}
		for _, key := range indexKeys {
			obj, _ := ctrl.GetIndexer().ByIndex(externalDNSHostnameIndex, strings.ToLower(key))
			objs = append(objs, obj...)
		}
		log.Debugf("Found %d matching DNSEndpoint objects", len(objs))
		for _, obj := range objs {
			dnsEndpoint, _ := obj.(*externaldnsv1.DNSEndpoint)

			for _, endpoint := range dnsEndpoint.Spec.Endpoints {
				for _, target := range endpoint.Targets {
					if endpoint.RecordType == "A" {
						addr, err := netip.ParseAddr(target)
						if err != nil {
							continue
						}
						if result["A"] == nil {
							result["A"] = make([]string, 0)
						}
						result["A"] = append(result["A"], addr.String())
					}
					if endpoint.RecordType == "AAAA" {
						addr, err := netip.ParseAddr(target)
						if err != nil {
							continue
						}
						if result["AAAA"] == nil {
							result["AAAA"] = make([]string, 0)
						}
						result["AAAA"] = append(result["AAAA"], addr.String())
					}
					if endpoint.RecordType == "TXT" {
						if result["TXT"] == nil {
							result["TXT"] = make([]string, 0)
						}
						result["TXT"] = append(result["TXT"], target)
					}
				}
			}
		}
		return result
	}
}

func fetchGatewayIPs(gw *gatewayapi_v1.Gateway) (results map[string][]string) {
	if results == nil {
		results = make(map[string][]string, 0)
	}

	for _, addr := range gw.Status.Addresses {
		if *addr.Type == gatewayapi_v1.IPAddressType {
			addr, err := netip.ParseAddr(addr.Value)
			if err != nil {
				continue
			}
			if addr.Is4() {
				if results["A"] == nil {
					results["A"] = make([]string, 0)
				}
				results["A"] = append(results["A"], addr.String())
			}
			if addr.Is6() {
				if results["AAAA"] == nil {
					results["AAAA"] = make([]string, 0)
				}
				results["AAAA"] = append(results["AAAA"], addr.String())
			}
			continue
		}

		if *addr.Type == gatewayapi_v1.HostnameAddressType {
			ips, err := net.LookupIP(addr.Value)
			if err != nil {
				continue
			}
			for _, ip := range ips {
				addr, err := netip.ParseAddr(ip.String())
				if err != nil {
					continue
				}
				if addr.Is4() {
					if results["A"] == nil {
						results["A"] = make([]string, 0)
					}
					results["A"] = append(results["A"], addr.String())
				}
				if addr.Is6() {
					if results["AAAA"] == nil {
						results["AAAA"] = make([]string, 0)
					}
					results["AAAA"] = append(results["AAAA"], addr.String())
				}
			}
		}
	}
	return
}

func fetchServiceLoadBalancerIPs(ingresses []core.LoadBalancerIngress) (results map[string][]string) {
	if results == nil {
		results = make(map[string][]string, 0)
	}

	for _, address := range ingresses {
		if address.Hostname != "" {
			log.Debugf("Looking up hostname %s", address.Hostname)
			ips, err := net.LookupIP(address.Hostname)
			if err != nil {
				continue
			}
			for _, ip := range ips {
				addr, err := netip.ParseAddr(ip.String())
				if err != nil {
					continue
				}
				if addr.Is4() {
					if results["A"] == nil {
						results["A"] = make([]string, 0)
					}
					results["A"] = append(results["A"], addr.String())
				}
				if addr.Is6() {
					if results["AAAA"] == nil {
						results["AAAA"] = make([]string, 0)
					}
					results["AAAA"] = append(results["AAAA"], addr.String())
				}
			}
		} else if address.IP != "" {
			addr, err := netip.ParseAddr(address.IP)
			if err != nil {
				continue
			}
			if addr.Is4() {
				if results["A"] == nil {
					results["A"] = make([]string, 0)
				}
				results["A"] = append(results["A"], addr.String())
			}
			if addr.Is6() {
				if results["AAAA"] == nil {
					results["AAAA"] = make([]string, 0)
				}
				results["AAAA"] = append(results["AAAA"], addr.String())
			}
		}
	}
	return
}

func fetchIngressLoadBalancerIPs(ingresses []networking.IngressLoadBalancerIngress) (results map[string][]string) {
	if results == nil {
		results = make(map[string][]string, 0)
	}

	for _, address := range ingresses {
		if address.Hostname != "" {
			log.Debugf("Looking up hostname %s", address.Hostname)
			ips, err := net.LookupIP(address.Hostname)
			if err != nil {
				continue
			}
			for _, ip := range ips {
				addr, err := netip.ParseAddr(ip.String())
				if err != nil {
					continue
				}
				if addr.Is4() {
					if results["A"] == nil {
						results["A"] = make([]string, 0)
					}
					results["A"] = append(results["A"], addr.String())
				}
				if addr.Is6() {
					if results["AAAA"] == nil {
						results["AAAA"] = make([]string, 0)
					}
					results["AAAA"] = append(results["AAAA"], addr.String())
				}
			}
		} else if address.IP != "" {
			addr, err := netip.ParseAddr(address.IP)
			if err != nil {
				continue
			}
			if addr.Is4() {
				if results["A"] == nil {
					results["A"] = make([]string, 0)
				}
				results["A"] = append(results["A"], addr.String())
			}
			if addr.Is6() {
				if results["AAAA"] == nil {
					results["AAAA"] = make([]string, 0)
				}
				results["AAAA"] = append(results["AAAA"], addr.String())
			}
		}
	}
	return
}

// the below is borrowed from k/k's GitHub repo
const (
	dns1123ValueFmt     string = "[a-z0-9]([-a-z0-9]*[a-z0-9])?"
	dns1123SubdomainFmt string = dns1123ValueFmt + "(\\." + dns1123ValueFmt + ")*"
)

var dns1123SubdomainRegexp = regexp.MustCompile("^" + dns1123SubdomainFmt + "$")

func isdns1123Hostname(value string) bool {
	return dns1123SubdomainRegexp.MatchString(value)
}

func appenddnsResults(result map[string][]string, fetchedResults map[string][]string) map[string][]string {
	if result == nil {
		result = make(map[string][]string, 0)
	}

	if result["A"] == nil {
		result["A"] = make([]string, 0)
	}
	result["A"] = append(result["A"], fetchedResults["A"]...)

	if result["AAAA"] == nil {
		result["AAAA"] = make([]string, 0)
	}
	result["AAAA"] = append(result["AAAA"], fetchedResults["AAAA"]...)

	if result["TXT"] == nil {
		result["TXT"] = make([]string, 0)
	}
	result["TXT"] = append(result["TXT"], fetchedResults["TXT"]...)

	return result
}
