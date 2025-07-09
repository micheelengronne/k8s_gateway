package gateway

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/fall"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
)

type lookupFunc func(indexKeys []string) map[string][]string

type resourceWithIndex struct {
	name   string
	lookup lookupFunc
}

// Static resources with their default noop function
var staticResources = []*resourceWithIndex{
	{name: "HTTPRoute", lookup: noop},
	{name: "TLSRoute", lookup: noop},
	{name: "GRPCRoute", lookup: noop},
	{name: "Ingress", lookup: noop},
	{name: "Service", lookup: noop},
	{name: "DNSEndpoint", lookup: noop},
}

var noop lookupFunc = func([]string) (result map[string][]string) { return }

var (
	ttlDefault        = uint32(60)
	ttlSOA            = uint32(60)
	defaultApex       = "dns1.kube-system"
	defaultHostmaster = "hostmaster"
	defaultSecondNS   = ""
)

// Gateway stores all runtime configuration of a plugin
type Gateway struct {
	Next                plugin.Handler
	Zones               []string
	Resources           []*resourceWithIndex
	ConfiguredResources []*string
	ttlLow              uint32
	ttlSOA              uint32
	Controller          *KubeController
	apex                string
	hostmaster          string
	secondNS            string
	configFile          string
	configContext       string
	ExternalAddrFunc    func(request.Request) []dns.RR
	resourceFilters     ResourceFilters

	Fall fall.F
}

type ResourceFilters struct {
	ingressClasses []string
	gatewayClasses []string
}

// Create a new Gateway instance
func newGateway() *Gateway {
	return &Gateway{
		Resources:           staticResources,
		ConfiguredResources: []*string{},
		ttlLow:              ttlDefault,
		ttlSOA:              ttlSOA,
		apex:                defaultApex,
		secondNS:            defaultSecondNS,
		hostmaster:          defaultHostmaster,
	}
}

func (gw *Gateway) lookupResource(resource string) *resourceWithIndex {
	for _, r := range gw.Resources {
		if r.name == resource {
			return r
		}
	}
	return nil
}

// Update resources in the Gateway based on provided configuration
func (gw *Gateway) updateResources(newResources []string) {
	log.Infof("updating resources with: %v", newResources)
	gw.Resources = nil // Clear existing resources

	// Create a map to hold enabled resources
	resourceLookup := make(map[string]*resourceWithIndex)

	// Fill the resource lookup map from static resources
	for _, resource := range staticResources {
		resourceLookup[resource.name] = resource
	}

	// Populate gw.Resources based on newResources
	for _, name := range newResources {
		if resource, exists := resourceLookup[name]; exists {
			log.Debugf("adding resource: %s", resource.name)
			gw.Resources = append(gw.Resources, resource)
		} else {
			log.Warningf("resource not found in static resources: %s", name)
		}
	}

	log.Debugf("final resources: %v", gw.Resources)
}

func (gw *Gateway) SetConfiguredResources(newResources []string) {
	gw.ConfiguredResources = make([]*string, len(newResources))
	for i, resource := range newResources {
		gw.ConfiguredResources[i] = &resource
	}
}

// ServeDNS implements the plugin.Handle interface.
func (gw *Gateway) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}
	//log.Infof("Incoming query %s", state.QName())

	qname := state.QName()
	zone := plugin.Zones(gw.Zones).Matches(qname)
	if zone == "" {
		log.Debugf("request %s has not matched any zones %v", qname, gw.Zones)
		return plugin.NextOrFailure(gw.Name(), gw.Next, ctx, w, r)
	}
	zone = qname[len(qname)-len(zone):] // maintain case of original query
	state.Zone = zone

	indexKeySets := gw.getQueryIndexKeySets(qname, zone)
	log.Debugf("computed Index Keys sets %v", indexKeySets)

	if !gw.Controller.HasSynced() {
		// TODO maybe there's a better way to do this? e.g. return an error back to the client?
		return dns.RcodeServerFailure, plugin.Error(thisPlugin, fmt.Errorf("could not sync required resources"))
	}

	var isRootZoneQuery bool
	for _, z := range gw.Zones {
		if state.Name() == z { // apex query
			isRootZoneQuery = true
			break
		}
		if dns.IsSubDomain(gw.apex+"."+z, state.Name()) {
			// dns subdomain test for ns. and dns. queries
			ret, err := gw.serveSubApex(state)
			return ret, err
		}
	}

	addrs := gw.getMatchingAddresses(indexKeySets)
	log.Debugf("computed response addresses %v", addrs)

	// Fall through if no host matches
	if len(addrs["A"]) == 0 && len(addrs["AAAA"]) == 0 && len(addrs["TXT"]) == 0 && gw.Fall.Through(qname) {
		return plugin.NextOrFailure(gw.Name(), gw.Next, ctx, w, r)
	}

	m := new(dns.Msg)
	m.SetReply(state.Req)

	switch state.QType() {
	case dns.TypeA:

		if len(addrs["A"]) == 0 {

			if !isRootZoneQuery {
				// No match, return NXDOMAIN
				m.Rcode = dns.RcodeNameError
			}

			m.Ns = []dns.RR{gw.soa(state)}

		} else {

			m.Answer = gw.A(state.Name(), addrs["A"])
		}

	case dns.TypeAAAA:

		if len(addrs["AAAA"]) == 0 {

			if !isRootZoneQuery {
				// No match, return NXDOMAIN
				m.Rcode = dns.RcodeNameError
			}

			// as per rfc4074 #3
			if len(addrs["A"]) > 0 {
				m.Rcode = dns.RcodeSuccess
			}

			m.Ns = []dns.RR{gw.soa(state)}

		} else {

			m.Answer = gw.AAAA(state.Name(), addrs["AAAA"])
		}

	case dns.TypeTXT:

		if len(addrs["TXT"]) == 0 {

			if !isRootZoneQuery {
				// No match, return NXDOMAIN
				m.Rcode = dns.RcodeNameError
			}

			m.Ns = []dns.RR{gw.soa(state)}

		} else {

			m.Answer = gw.TXT(state.Name(), addrs["TXT"])
		}

	case dns.TypeSOA:

		m.Answer = []dns.RR{gw.soa(state)}

	case dns.TypeNS:

		if isRootZoneQuery {
			m.Answer = gw.nameservers(state)

			addr := gw.ExternalAddrFunc(state)
			for _, rr := range addr {
				rr.Header().Ttl = gw.ttlSOA
				m.Extra = append(m.Extra, rr)
			}
		} else {
			m.Ns = []dns.RR{gw.soa(state)}
		}

	default:
		m.Ns = []dns.RR{gw.soa(state)}
	}

	// Force to true to fix broken behaviour of legacy glibc `getaddrinfo`.
	// See https://github.com/coredns/coredns/pull/3573
	m.Authoritative = true

	if err := w.WriteMsg(m); err != nil {
		log.Errorf("failed to send a response: %s", err)
	}

	return dns.RcodeSuccess, nil
}

// Computes keys to look up in cache
func (gw *Gateway) getQueryIndexKeys(qName, zone string) []string {
	zonelessQuery := stripDomain(qName, zone)

	var indexKeys []string
	strippedQName := stripClosingDot(qName)
	if len(zonelessQuery) != 0 && zonelessQuery != strippedQName {
		indexKeys = []string{strippedQName, zonelessQuery}
	} else {
		indexKeys = []string{strippedQName}
	}

	return indexKeys
}

// Returns all sets of index keys that should be checked, in order, for a given
// query name and zone. The first set of keys is the most specific, and the last
// set is the most general. The first set of keys that is in the indexer should
// be used to look up the query.
func (gw *Gateway) getQueryIndexKeySets(qName, zone string) [][]string {
	specificIndexKeys := gw.getQueryIndexKeys(qName, zone)

	wildcardQName := gw.toWildcardQName(qName, zone)
	if wildcardQName == "" {
		return [][]string{specificIndexKeys}
	}

	wildcardIndexKeys := gw.getQueryIndexKeys(wildcardQName, zone)
	return [][]string{specificIndexKeys, wildcardIndexKeys}
}

// Converts a query name to a wildcard query name by replacing the first
// label with a wildcard. The wildcard query name is used to look up
// wildcard records in the indexer. If the query name is empty or
// contains no labels, an empty string is returned.
func (gw *Gateway) toWildcardQName(qName, zone string) string {
	// Indexer cache can be built from `name.namespace` without zone
	zonelessQuery := stripDomain(qName, zone)
	parts := strings.Split(zonelessQuery, ".")
	if len(parts) == 0 {
		return ""
	}

	parts[0] = "*"
	parts = append(parts, zone)
	return strings.Join(parts, ".")
}

// Gets the set of addresses associated with the first set of index keys
// that is in the indexer.
func (gw *Gateway) getMatchingAddresses(indexKeySets [][]string) map[string][]string {
	// Iterate over supported resources and lookup DNS queries
	// Stop once we've found at least one match
	for _, indexKeys := range indexKeySets {
		for _, resource := range gw.Resources {
			addrs := resource.lookup(indexKeys)

			if addrs == nil {
				addrs = make(map[string][]string, 0)
			}
			if addrs["A"] == nil {
				addrs["A"] = make([]string, 0)
			}
			if addrs["AAAA"] == nil {
				addrs["AAAA"] = make([]string, 0)
			}
			if addrs["TXT"] == nil {
				addrs["TXT"] = make([]string, 0)
			}

			if len(addrs["A"]) > 0 {
				return addrs
			}
			if len(addrs["AAAA"]) > 0 {
				return addrs
			}
			if len(addrs["TXT"]) > 0 {
				return addrs
			}
		}
	}

	return nil
}

// Name implements the Handler interface.
func (gw *Gateway) Name() string { return thisPlugin }

// A does the A-record lookup in ingress indexer
func (gw *Gateway) A(name string, results []string) (records []dns.RR) {
	dup := make(map[string]struct{})
	for _, result := range results {
		if _, ok := dup[result]; !ok {
			dup[result] = struct{}{}
			records = append(records, &dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: gw.ttlLow}, A: net.ParseIP(result)})
		}
	}
	return records
}

func (gw *Gateway) AAAA(name string, results []string) (records []dns.RR) {
	dup := make(map[string]struct{})
	for _, result := range results {
		if _, ok := dup[result]; !ok {
			dup[result] = struct{}{}
			records = append(records, &dns.AAAA{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: gw.ttlLow}, AAAA: net.ParseIP(result)})
		}
	}
	return records
}

func (gw *Gateway) TXT(name string, results []string) (records []dns.RR) {
	dup := make(map[string]struct{})
	for _, result := range results {
		if _, ok := dup[result]; !ok {
			dup[result] = struct{}{}
			records = append(records, &dns.TXT{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: gw.ttlLow}, Txt: split255(result)})
		}
	}

	return records
}

// SelfAddress returns the address of the local k8s_gateway service
func (gw *Gateway) SelfAddress(state request.Request) (records []dns.RR) {

	var addrs1, addrs2 []string
	for _, resource := range gw.Resources {
		results := resource.lookup([]string{gw.apex})
		if len(results["A"]) > 0 {
			addrs1 = append(addrs1, results["A"]...)
		}
		if len(results["AAAA"]) > 0 {
			addrs1 = append(addrs1, results["AAAA"]...)
		}
		results = resource.lookup([]string{gw.secondNS})
		if len(results["A"]) > 0 {
			addrs2 = append(addrs2, results["A"]...)
		}
		if len(results["AAAA"]) > 0 {
			addrs2 = append(addrs2, results["AAAA"]...)
		}
	}

	records = append(records, gw.A(gw.apex+"."+state.Zone, addrs1)...)

	if state.QType() == dns.TypeNS {
		records = append(records, gw.A(gw.secondNS+"."+state.Zone, addrs2)...)
	}

	return records
	//return records
}

// Strips the zone from FQDN and return a hostname
func stripDomain(qname, zone string) string {
	hostname := qname[:len(qname)-len(zone)]
	return stripClosingDot(hostname)
}

// Strips the closing dot unless it's "."
func stripClosingDot(s string) string {
	if len(s) > 1 {
		return strings.TrimSuffix(s, ".")
	}
	return s
}

// src: https://github.com/coredns/coredns/blob/0aee758833cabb1ec89756a698c52b83bbbdc587/plugin/etcd/msg/service.go#L145
// Split255 splits a string into 255 byte chunks.
func split255(s string) []string {
	if len(s) < 255 {
		return []string{s}
	}
	sx := []string{}
	p, i := 0, 255
	for {
		if i > len(s) {
			sx = append(sx, s[p:])
			break
		}
		sx = append(sx, s[p:i])
		p, i = p+255, i+255
	}

	return sx
}
