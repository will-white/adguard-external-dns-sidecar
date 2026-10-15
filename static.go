package main

import (
	"fmt"
	"net"
	"strings"
)

// StaticRecord is one fixed name -> address rewrite owned by the managed block.
// It exists for hosts that live outside the cluster (a reverse proxy on the
// router, an appliance) and so never show up as an Ingress.
type StaticRecord struct {
	Host string // fully qualified, always under ROOT_DOMAIN
	IP   net.IP
}

// RRType is the DNS record type AdGuard should answer with for this address.
func (r StaticRecord) RRType() string {
	if r.IP.To4() != nil {
		return "A"
	}
	return "AAAA"
}

// Rule renders the record as an exact-host AdGuard rewrite. The `|...^` anchors
// match the host itself and nothing beneath it, the same form external-dns's
// AdGuard provider writes, and `important` keeps blocklists from overriding it.
func (r StaticRecord) Rule() string {
	return fmt.Sprintf("|%s^$dnsrewrite=NOERROR;%s;%s,important", r.Host, r.RRType(), r.IP.String())
}

// parseStaticRecords reads STATIC_RECORDS: comma-separated `name=ip` pairs.
// A bare label (`nas`) is qualified with the root domain; a name already under
// the root domain is kept as is. Anything else is rejected, because the managed
// block must only ever touch names under ROOT_DOMAIN.
func parseStaticRecords(raw, rootDomain string) ([]StaticRecord, error) {
	var out []StaticRecord
	seen := make(map[string]bool)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, addr, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("static record %q: want name=ip", part)
		}
		name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
		addr = strings.TrimSpace(addr)
		if name == "" {
			return nil, fmt.Errorf("static record %q: empty name", part)
		}
		if strings.ContainsAny(name, " |^$,/*") {
			return nil, fmt.Errorf("static record %q: invalid characters in name", part)
		}
		host := name
		switch {
		case name == rootDomain || strings.HasSuffix(name, "."+rootDomain):
		case strings.Contains(name, "."):
			return nil, fmt.Errorf("static record %q: %q is not under %s", part, name, rootDomain)
		default:
			host = name + "." + rootDomain
		}
		ip := net.ParseIP(addr)
		if ip == nil {
			return nil, fmt.Errorf("static record %q: %q is not an IP address", part, addr)
		}
		key := host + "/" + StaticRecord{IP: ip}.RRType()
		if seen[key] {
			return nil, fmt.Errorf("static record %q: duplicate %s record for %s", part, StaticRecord{IP: ip}.RRType(), host)
		}
		seen[key] = true
		out = append(out, StaticRecord{Host: host, IP: ip})
	}
	return out, nil
}
