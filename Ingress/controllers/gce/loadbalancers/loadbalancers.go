/*
Copyright 2015 The Kubernetes Authors All rights reserved.

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

package loadbalancers

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	compute "google.golang.org/api/compute/v1"
	"k8s.io/contrib/Ingress/controllers/gce/backends"
	"k8s.io/contrib/Ingress/controllers/gce/storage"
	"k8s.io/contrib/Ingress/controllers/gce/utils"
	"k8s.io/kubernetes/pkg/util/sets"

	"github.com/golang/glog"
)

const (

	// The gce api uses the name of a path rule to match a host rule.
	hostRulePrefix = "host"

	// DefaultHost is the host used if none is specified. It is a valid value
	// for the "Host" field recognized by GCE.
	DefaultHost = "*"

	// DefaultPath is the path used if none is specified. It is a valid path
	// recognized by GCE.
	DefaultPath = "/*"

	// A single target proxy/urlmap/forwarding rule is created per loadbalancer.
	// Tagged with the namespace/name of the Ingress.
	targetProxyPrefix         = "k8s-tp"
	targetHTTPSProxyPrefix    = "k8s-tps"
	sslCertPrefix             = "k8s-ssl"
	forwardingRulePrefix      = "k8s-fw"
	httpsForwardingRulePrefix = "k8s-fws"
	urlMapPrefix              = "k8s-um"
	defaultPortRange          = "80"
	httpsDefaultPortRange     = "443"
)

// L7s implements LoadBalancerPool.
type L7s struct {
	cloud       LoadBalancers
	snapshotter storage.Snapshotter
	// TODO: Remove this field and always ask the BackendPool using the NodePort.
	glbcDefaultBackend     *compute.BackendService
	defaultBackendPool     backends.BackendPool
	defaultBackendNodePort int64
	namer                  utils.Namer
}

// NewLoadBalancerPool returns a new loadbalancer pool.
// - cloud: implements LoadBalancers. Used to sync L7 loadbalancer resources
//	 with the cloud.
// - defaultBackendPool: a BackendPool used to manage the GCE BackendService for
//   the default backend.
// - defaultBackendNodePort: The nodePort of the Kubernetes service representing
//   the default backend.
func NewLoadBalancerPool(
	cloud LoadBalancers,
	defaultBackendPool backends.BackendPool,
	defaultBackendNodePort int64, namer utils.Namer) LoadBalancerPool {
	return &L7s{cloud, storage.NewInMemoryPool(), nil, defaultBackendPool, defaultBackendNodePort, namer}
}

func (l *L7s) create(ri *L7RuntimeInfo) (*L7, error) {
	// Lazily create a default backend so we don't tax users who don't care
	// about Ingress by consuming 1 of their 3 GCE BackendServices. This
	// BackendService is deleted when there are no more Ingresses, either
	// through Sync or Shutdown.
	if l.glbcDefaultBackend == nil {
		err := l.defaultBackendPool.Add(l.defaultBackendNodePort)
		if err != nil {
			return nil, err
		}
		l.glbcDefaultBackend, err = l.defaultBackendPool.Get(l.defaultBackendNodePort)
		if err != nil {
			return nil, err
		}
	}
	return &L7{
		runtimeInfo:        ri,
		Name:               l.namer.LBName(ri.Name),
		cloud:              l.cloud,
		glbcDefaultBackend: l.glbcDefaultBackend,
		namer:              l.namer,
		sslCert:            nil,
	}, nil
}

// Get returns the loadbalancer by name.
func (l *L7s) Get(name string) (*L7, error) {
	name = l.namer.LBName(name)
	lb, exists := l.snapshotter.Get(name)
	if !exists {
		return nil, fmt.Errorf("Loadbalancer %v not in pool", name)
	}
	return lb.(*L7), nil
}

// Add gets or creates a loadbalancer.
// If the loadbalancer already exists, it checks that its edges are valid.
func (l *L7s) Add(ri *L7RuntimeInfo) (err error) {
	name := l.namer.LBName(ri.Name)

	lb, _ := l.Get(name)
	if lb == nil {
		glog.Infof("Creating l7 %v", name)
		lb, err = l.create(ri)
		if err != nil {
			return err
		}
	}
	// Add the lb to the pool, in case we create an UrlMap but run out
	// of quota in creating the ForwardingRule we still need to cleanup
	// the UrlMap during GC.
	defer l.snapshotter.Add(name, lb)

	// Why edge hop for the create?
	// The loadbalancer is a fictitious resource, it doesn't exist in gce. To
	// make it exist we need to create a collection of gce resources, done
	// through the edge hop.
	if err := lb.edgeHop(); err != nil {
		return err
	}

	return nil
}

// Delete deletes a loadbalancer by name.
func (l *L7s) Delete(name string) error {
	name = l.namer.LBName(name)
	lb, err := l.Get(name)
	if err != nil {
		return err
	}
	glog.Infof("Deleting lb %v", lb.runtimeInfo.Name)
	if err := lb.Cleanup(); err != nil {
		return err
	}
	l.snapshotter.Delete(lb.runtimeInfo.Name)
	return nil
}

// Sync loadbalancers with the given runtime info from the controller.
func (l *L7s) Sync(lbs []*L7RuntimeInfo) error {
	glog.V(3).Infof("Creating loadbalancers %+v", lbs)

	// The default backend is completely managed by the l7 pool.
	// This includes recreating it if it's deleted, or fixing broken links.
	if err := l.defaultBackendPool.Sync([]int64{l.defaultBackendNodePort}); err != nil {
		return err
	}
	// create new loadbalancers, perform an edge hop for existing
	for _, ri := range lbs {
		if err := l.Add(ri); err != nil {
			return err
		}
	}
	// Tear down the default backend when there are no more loadbalancers
	// because the cluster could go down anytime and we'd leak it otherwise.
	if len(lbs) == 0 {
		if err := l.defaultBackendPool.Delete(l.defaultBackendNodePort); err != nil {
			return err
		}
		l.glbcDefaultBackend = nil
	}
	return nil
}

// GC garbage collects loadbalancers not in the input list.
func (l *L7s) GC(names []string) error {
	knownLoadBalancers := sets.NewString()
	for _, n := range names {
		knownLoadBalancers.Insert(l.namer.LBName(n))
	}
	pool := l.snapshotter.Snapshot()

	// Delete unknown loadbalancers
	for name := range pool {
		if knownLoadBalancers.Has(name) {
			continue
		}
		glog.Infof("GCing loadbalancer %v", name)
		if err := l.Delete(name); err != nil {
			return err
		}
	}
	return nil
}

// Shutdown logs whether or not the pool is empty.
func (l *L7s) Shutdown() error {
	if err := l.GC([]string{}); err != nil {
		return err
	}
	if err := l.defaultBackendPool.Shutdown(); err != nil {
		return err
	}
	glog.Infof("Loadbalancer pool shutdown.")
	return nil
}

// TLSCerts encapsulates .pem encoded TLS information.
type TLSCerts struct {
	// Key is private key
	Key string
	// Cert is a public key
	Cert string
	// Chain is a certificate chain.
	Chain string
}

// L7RuntimeInfo is info passed to this module from the controller runtime.
type L7RuntimeInfo struct {
	// Name is the name of a loadbalancer
	Name string
	// IP is the desired ip of the loadbalancer, eg from a staticIP
	IP string
	// TLS are the tls certs to use in termination.
	TLS *TLSCerts
	// BlockHTTP will not setup :80, if TLS is nil and BlockHTTP is set,
	// no loadbalancer is created.
	BlockHTTP bool
}

// L7 represents a single L7 loadbalancer.
type L7 struct {
	Name        string
	runtimeInfo *L7RuntimeInfo
	cloud       LoadBalancers
	um          *compute.UrlMap
	tp          *compute.TargetHttpProxy
	tps         *compute.TargetHttpsProxy
	fw          *compute.ForwardingRule
	fws         *compute.ForwardingRule
	ip          *compute.Address
	// TODO: Make this a custom type that contains crt+key
	sslCert *compute.SslCertificate
	// This is the backend to use if no path rules match
	// TODO: Expose this to users.
	glbcDefaultBackend *compute.BackendService
	// namer is used to compute names of the various sub-components of an L7.
	namer utils.Namer
}

func (l *L7) checkUrlMap(backend *compute.BackendService) (err error) {
	if l.glbcDefaultBackend == nil {
		return fmt.Errorf("Cannot create urlmap without default backend.")
	}
	urlMapName := l.namer.Truncate(fmt.Sprintf("%v-%v", urlMapPrefix, l.Name))
	urlMap, _ := l.cloud.GetUrlMap(urlMapName)
	if urlMap != nil {
		glog.V(3).Infof("Url map %v already exists", urlMap.Name)
		l.um = urlMap
		return nil
	}

	glog.Infof("Creating url map %v for backend %v", urlMapName, l.glbcDefaultBackend.Name)
	urlMap, err = l.cloud.CreateUrlMap(l.glbcDefaultBackend, urlMapName)
	if err != nil {
		return err
	}
	l.um = urlMap
	return nil
}

func (l *L7) checkProxy() (err error) {
	if l.um == nil {
		return fmt.Errorf("Cannot create proxy without urlmap.")
	}
	if l.runtimeInfo.BlockHTTP {
		glog.Infof("Not setting up an HTTP Proxy for %v, BlockHTTP requested", l.Name)
		return nil
	}
	proxyName := l.namer.Truncate(fmt.Sprintf("%v-%v", targetProxyPrefix, l.Name))
	proxy, _ := l.cloud.GetTargetHttpProxy(proxyName)
	if proxy == nil {
		glog.Infof("Creating new http proxy for urlmap %v", l.um.Name)
		proxy, err = l.cloud.CreateTargetHttpProxy(l.um, proxyName)
		if err != nil {
			return err
		}
		l.tp = proxy
		return nil
	}
	if !utils.CompareLinks(proxy.UrlMap, l.um.SelfLink) {
		glog.Infof("Proxy %v has the wrong url map, setting %v overwriting %v",
			proxy.Name, l.um, proxy.UrlMap)
		if err := l.cloud.SetUrlMapForTargetHttpProxy(proxy, l.um); err != nil {
			return err
		}
	}
	l.tp = proxy
	return nil
}

func (l *L7) checkSSLCert() (err error) {
	if l.runtimeInfo.TLS == nil {
		glog.V(3).Infof("No SSL certificates for %v", l.Name)
		return nil
	}
	// TODO: Currently, GCE only supports a single certificate per static IP
	// so we don't need to bother with disambiguation. Naming the cert after
	// the loadbalancer is a simplification.
	certName := l.namer.Truncate(fmt.Sprintf("%v-%v", sslCertPrefix, l.Name))
	cert, _ := l.cloud.GetSslCertificate(certName)
	if cert == nil {
		glog.Infof("Creating new sslCertificates %v for %v", l.Name, certName)
		cert, err = l.cloud.CreateSslCertificates(&compute.SslCertificate{
			Name:        certName,
			Certificate: l.runtimeInfo.TLS.Cert,
			PrivateKey:  l.runtimeInfo.TLS.Key,
		})
		if err != nil {
			return err
		}
	}
	l.sslCert = cert
	// TODO: V(3)
	glog.Infof("Found ssl certificates associated with name: %v", l.sslCert.Name)
	return nil
}

func (l *L7) checkHttpsProxy() (err error) {
	if l.sslCert == nil {
		glog.V(3).Infof("No SSL certificates for %v, will not create HTTPS proxy.", l.Name)
		return nil
	}
	if l.um == nil {
		return fmt.Errorf("No UrlMap for %v, will not create HTTPS proxy.", l.Name)
	}
	proxyName := l.namer.Truncate(fmt.Sprintf("%v-%v", targetHTTPSProxyPrefix, l.Name))
	proxy, _ := l.cloud.GetTargetHttpsProxy(proxyName)
	if proxy == nil {
		glog.Infof("Creating new https proxy for urlmap %v", l.um.Name)
		proxy, err = l.cloud.CreateTargetHttpsProxy(l.um, l.sslCert, proxyName)
		if err != nil {
			return err
		}
		l.tps = proxy
		return nil
	}
	if !utils.CompareLinks(proxy.UrlMap, l.um.SelfLink) {
		glog.Infof("Https proxy %v has the wrong url map, setting %v overwriting %v",
			proxy.Name, l.um, proxy.UrlMap)
		if err := l.cloud.SetUrlMapForTargetHttpsProxy(proxy, l.um); err != nil {
			return err
		}
	}
	cert := proxy.SslCertificates[0]
	if !utils.CompareLinks(cert, l.sslCert.SelfLink) {
		glog.Infof("Https proxy %v has the wrong ssl certs, setting %v overwriting %v",
			proxy.Name, l.sslCert.SelfLink, cert)
		if err := l.cloud.SetSslCertificateForTargetHttpsProxy(proxy, l.sslCert); err != nil {
			return err
		}
	}
	glog.Infof("Created target https proxy %v", proxy.Name)
	l.tps = proxy
	return nil
}

func (l *L7) checkForwardingRule() (err error) {
	if l.tp == nil {
		return fmt.Errorf("Cannot create forwarding rule without proxy.")
	}
	var address string
	if l.ip != nil {
		address = l.ip.Address
	}
	forwardingRuleName := l.namer.Truncate(fmt.Sprintf("%v-%v", forwardingRulePrefix, l.Name))
	fw, _ := l.cloud.GetGlobalForwardingRule(forwardingRuleName)
	if fw != nil && fw.IPAddress != address {
		glog.Warningf("Loadbalancer IP %v is not %v")
		if err := l.cloud.DeleteGlobalForwardingRule(forwardingRuleName); err != nil {
			if !utils.IsHTTPErrorCode(err, http.StatusNotFound) {
				return err
			}
		}
		fw = nil
	}
	if fw == nil {
		glog.Infof("Creating forwarding rule for proxy %v", l.tp.Name)
		fw, err = l.cloud.CreateGlobalForwardingRule(
			l.tp, address, forwardingRuleName, defaultPortRange)
		if err != nil {
			return err
		}
		l.fw = fw
		return nil
	}
	// TODO: If the port range and protocol don't match, recreate the rule
	if utils.CompareLinks(fw.Target, l.tp.SelfLink) {
		glog.Infof("Forwarding rule %v already exists", fw.Name)
		l.fw = fw
		return nil
	}
	glog.Infof("Forwarding rule %v has the wrong proxy, setting %v overwriting %v",
		fw.Name, fw.Target, l.tp.SelfLink)
	if err := l.cloud.SetProxyForGlobalForwardingRule(fw, l.tp); err != nil {
		return err
	}
	l.fw = fw
	return nil
}

func (l *L7) checkHttpsForwardingRule() (err error) {
	if l.tps == nil {
		glog.V(3).Infof("No https target proxy for %v, not created https forwarding rule", l.Name)
		return nil
	}
	var address string
	if l.ip != nil {
		address = l.ip.Address
	}
	forwardingRuleName := l.namer.Truncate(fmt.Sprintf("%v-%v", httpsForwardingRulePrefix, l.Name))
	fws, _ := l.cloud.GetGlobalForwardingRule(forwardingRuleName)
	// The only way to get in this state is to delete and create the forwarding
	// rule, since we're not allowed to update it.
	if fws != nil && fws.IPAddress != address {
		glog.Warningf("Loadbalancer IP %v is not %v")
		if err := l.cloud.DeleteGlobalForwardingRule(forwardingRuleName); err != nil {
			if !utils.IsHTTPErrorCode(err, http.StatusNotFound) {
				return err
			}
		}
		fws = nil
	}
	if fws == nil {
		glog.Infof("Creating forwarding rule for proxy %v", l.tps.Name)
		fws, err = l.cloud.CreateGlobalForwardingRule(
			&compute.TargetHttpProxy{SelfLink: l.tps.SelfLink}, address, forwardingRuleName, httpsDefaultPortRange)
		if err != nil {
			return err
		}
	}
	// TODO: If the port range and protocol don't match, recreate the rule
	if utils.CompareLinks(fws.Target, l.tps.SelfLink) {
		glog.Infof("Forwarding rule %v already exists", fws.Name)
		l.fws = fws
		return nil
	}
	glog.Infof("Forwarding rule %v has the wrong proxy, setting %v overwriting %v",
		fws.Name, fws.Target, l.tps.SelfLink)
	if err := l.cloud.SetProxyForGlobalForwardingRule(fws, &compute.TargetHttpProxy{SelfLink: l.tps.SelfLink}); err != nil {
		return err
	}
	l.fws = fws
	return nil
}

func (l *L7) checkStaticIP() (err error) {
	if l.fw == nil || l.fw.IPAddress == "" {
		return fmt.Errorf("Will not create static IP without a forwarding rule.")
	}
	staticIPName := l.namer.Truncate(fmt.Sprintf("%v-%v", forwardingRulePrefix, l.Name))
	ip, _ := l.cloud.GetGlobalStaticIP(staticIPName)
	if ip == nil {
		glog.Infof("Creating static ip %v", staticIPName)
		ip, err = l.cloud.AllocateGlobalStaticIP(staticIPName, l.fw.IPAddress)
		if err != nil {
			if utils.IsHTTPErrorCode(err, http.StatusConflict) ||
				utils.IsHTTPErrorCode(err, http.StatusBadRequest) {
				glog.Infof("IP %v(%v) is already reserved, assuming it is OK to use.",
					l.fw.IPAddress, staticIPName)
				return nil
			}
			return err
		}
	}
	l.ip = ip
	return nil
}

func (l *L7) edgeHop() error {
	if err := l.checkUrlMap(l.glbcDefaultBackend); err != nil {
		return err
	}
	if err := l.checkProxy(); err != nil {
		return err
	}
	if err := l.checkSSLCert(); err != nil {
		return err
	}
	if err := l.checkHttpsProxy(); err != nil {
		return err
	}
	// Ordering is important. We create the global forwarding rule
	// promote its IP, and create the second rule with the same IP.
	if err := l.checkForwardingRule(); err != nil {
		return err
	}
	if err := l.checkStaticIP(); err != nil {
		return err
	}
	if err := l.checkHttpsForwardingRule(); err != nil {
		return err
	}
	return nil
}

// GetIP returns the ip associated with the forwarding rule for this l7.
func (l *L7) GetIP() string {
	return l.fw.IPAddress
}

// getNameForPathMatcher returns a name for a pathMatcher based on the given host rule.
// The host rule can be a regex, the path matcher name used to associate the 2 cannot.
func getNameForPathMatcher(hostRule string) string {
	hasher := md5.New()
	hasher.Write([]byte(hostRule))
	return fmt.Sprintf("%v%v", hostRulePrefix, hex.EncodeToString(hasher.Sum(nil)))
}

// UpdateUrlMap translates the given hostname: endpoint->port mapping into a gce url map.
//
// HostRule: Conceptually contains all PathRules for a given host.
// PathMatcher: Associates a path rule with a host rule. Mostly an optimization.
// PathRule: Maps a single path regex to a backend.
//
// The GCE url map allows multiple hosts to share url->backend mappings without duplication, eg:
//   Host: foo(PathMatcher1), bar(PathMatcher1,2)
//   PathMatcher1:
//     /a -> b1
//     /b -> b2
//   PathMatcher2:
//     /c -> b1
// This leads to a lot of complexity in the common case, where all we want is a mapping of
// host->{/path: backend}.
//
// Consider some alternatives:
// 1. Using a single backend per PathMatcher:
//   Host: foo(PathMatcher1,3) bar(PathMatcher1,2,3)
//   PathMatcher1:
//     /a -> b1
//   PathMatcher2:
//     /c -> b1
//   PathMatcher3:
//     /b -> b2
// 2. Using a single host per PathMatcher:
//   Host: foo(PathMatcher1)
//   PathMatcher1:
//     /a -> b1
//     /b -> b2
//   Host: bar(PathMatcher2)
//   PathMatcher2:
//     /a -> b1
//     /b -> b2
//     /c -> b1
// In the context of kubernetes services, 2 makes more sense, because we
// rarely want to lookup backends (service:nodeport). When a service is
// deleted, we need to find all host PathMatchers that have the backend
// and remove the mapping. When a new path is added to a host (happens
// more frequently than service deletion) we just need to lookup the 1
// pathmatcher of the host.
func (l *L7) UpdateUrlMap(ingressRules utils.GCEURLMap) error {
	if l.um == nil {
		return fmt.Errorf("Cannot add url without an urlmap.")
	}
	glog.V(3).Infof("Updating urlmap for l7 %v", l.Name)

	// All UrlMaps must have a default backend. If the Ingress has a default
	// backend, it applies to all host rules as well as to the urlmap itself.
	// If it doesn't the urlmap might have a stale default, so replace it with
	// glbc's default backend.
	defaultBackend := ingressRules.GetDefaultBackend()
	if defaultBackend != nil {
		l.um.DefaultService = defaultBackend.SelfLink
	} else {
		l.um.DefaultService = l.glbcDefaultBackend.SelfLink
	}
	glog.V(3).Infof("Updating url map %+v", ingressRules)

	for hostname, urlToBackend := range ingressRules {
		// Find the hostrule
		// Find the path matcher
		// Add all given endpoint:backends to pathRules in path matcher
		var hostRule *compute.HostRule
		pmName := getNameForPathMatcher(hostname)
		for _, hr := range l.um.HostRules {
			// TODO: Hostnames must be exact match?
			if hr.Hosts[0] == hostname {
				hostRule = hr
				break
			}
		}
		if hostRule == nil {
			// This is a new host
			hostRule = &compute.HostRule{
				Hosts:       []string{hostname},
				PathMatcher: pmName,
			}
			// Why not just clobber existing host rules?
			// Because we can have multiple loadbalancers point to a single
			// gce url map when we have IngressClaims.
			l.um.HostRules = append(l.um.HostRules, hostRule)
		}
		var pathMatcher *compute.PathMatcher
		for _, pm := range l.um.PathMatchers {
			if pm.Name == hostRule.PathMatcher {
				pathMatcher = pm
				break
			}
		}
		if pathMatcher == nil {
			// This is a dangling or new host
			pathMatcher = &compute.PathMatcher{Name: pmName}
			l.um.PathMatchers = append(l.um.PathMatchers, pathMatcher)
		}
		pathMatcher.DefaultService = l.um.DefaultService

		// TODO: Every update replaces the entire path map. This will need to
		// change when we allow joining. Right now we call a single method
		// to verify current == desired and add new url mappings.
		pathMatcher.PathRules = []*compute.PathRule{}

		// Longest prefix wins. For equal rules, first hit wins, i.e the second
		// /foo rule when the first is deleted.
		for expr, be := range urlToBackend {
			pathMatcher.PathRules = append(
				pathMatcher.PathRules, &compute.PathRule{[]string{expr}, be.SelfLink})
		}
	}
	um, err := l.cloud.UpdateUrlMap(l.um)
	if err != nil {
		return err
	}
	l.um = um
	return nil
}

// Cleanup deletes resources specific to this l7 in the right order.
// forwarding rule -> target proxy -> url map
// This leaves backends and health checks, which are shared across loadbalancers.
func (l *L7) Cleanup() error {
	if l.fw != nil {
		glog.Infof("Deleting global forwarding rule %v", l.fw.Name)
		if err := l.cloud.DeleteGlobalForwardingRule(l.fw.Name); err != nil {
			if !utils.IsHTTPErrorCode(err, http.StatusNotFound) {
				return err
			}
		}
		l.fw = nil
	}
	if l.fws != nil {
		glog.Infof("Deleting global forwarding rule %v", l.fws.Name)
		if err := l.cloud.DeleteGlobalForwardingRule(l.fws.Name); err != nil {
			if !utils.IsHTTPErrorCode(err, http.StatusNotFound) {
				return err
			}
			l.fws = nil
		}
	}
	if l.ip != nil {
		glog.Infof("Deleting static IP %v(%v)", l.ip.Name, l.ip.Address)
		if err := l.cloud.DeleteGlobalStaticIP(l.ip.Name); err != nil {
			if !utils.IsHTTPErrorCode(err, http.StatusNotFound) {
				return err
			}
			l.ip = nil
		}
	}
	if l.tps != nil {
		glog.Infof("Deleting target https proxy %v", l.tps.Name)
		if err := l.cloud.DeleteTargetHttpsProxy(l.tps.Name); err != nil {
			if !utils.IsHTTPErrorCode(err, http.StatusNotFound) {
				return err
			}
		}
		l.tps = nil
	}
	if l.sslCert != nil {
		glog.Infof("Deleting sslcert %v", l.sslCert.Name)
		if err := l.cloud.DeleteSslCertificates(l.sslCert.Name); err != nil {
			if !utils.IsHTTPErrorCode(err, http.StatusNotFound) {
				return err
			}
		}
		l.sslCert = nil
	}
	if l.tp != nil {
		glog.Infof("Deleting target proxy %v", l.tp.Name)
		if err := l.cloud.DeleteTargetHttpProxy(l.tp.Name); err != nil {
			if !utils.IsHTTPErrorCode(err, http.StatusNotFound) {
				return err
			}
		}
		l.tp = nil
	}
	if l.um != nil {
		glog.Infof("Deleting url map %v", l.um.Name)
		if err := l.cloud.DeleteUrlMap(l.um.Name); err != nil {
			if !utils.IsHTTPErrorCode(err, http.StatusNotFound) {
				return err
			}
		}
		l.um = nil
	}
	return nil
}

// getBackendNames returns the names of backends in this L7 urlmap.
func (l *L7) getBackendNames() []string {
	if l.um == nil {
		return []string{}
	}
	beNames := sets.NewString()
	for _, pathMatcher := range l.um.PathMatchers {
		for _, pathRule := range pathMatcher.PathRules {
			// This is gross, but the urlmap only has links to backend services.
			parts := strings.Split(pathRule.Service, "/")
			name := parts[len(parts)-1]
			if name != "" {
				beNames.Insert(name)
			}
		}
	}
	// The default Service recorded in the urlMap is a link to the backend.
	// Note that this can either be user specified, or the L7 controller's
	// global default.
	parts := strings.Split(l.um.DefaultService, "/")
	defaultBackendName := parts[len(parts)-1]
	if defaultBackendName != "" {
		beNames.Insert(defaultBackendName)
	}
	return beNames.List()
}

// GetLBAnnotations returns the annotations of an l7. This includes it's current status.
func GetLBAnnotations(l7 *L7, existing map[string]string, backendPool backends.BackendPool) map[string]string {
	if existing == nil {
		existing = map[string]string{}
	}
	backends := l7.getBackendNames()
	backendState := map[string]string{}
	for _, beName := range backends {
		backendState[beName] = backendPool.Status(beName)
	}
	jsonBackendState := "Unknown"
	b, err := json.Marshal(backendState)
	if err == nil {
		jsonBackendState = string(b)
	}
	existing[fmt.Sprintf("%v/url-map", utils.K8sAnnotationPrefix)] = l7.um.Name
	existing[fmt.Sprintf("%v/forwarding-rule", utils.K8sAnnotationPrefix)] = l7.fw.Name
	existing[fmt.Sprintf("%v/target-proxy", utils.K8sAnnotationPrefix)] = l7.tp.Name
	// TODO: We really want to know when a backend flipped states.
	existing[fmt.Sprintf("%v/backends", utils.K8sAnnotationPrefix)] = jsonBackendState
	return existing
}
