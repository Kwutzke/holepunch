package resolver

import (
	"net"
	"os"

	mdns "github.com/miekg/dns"
)

// systemd-resolved's /etc/resolv.conf names only its stub, which routes the TLDs holepunch claims
// back to holepunch; the file it keeps under /run lists the real upstreams.
var resolvConfCandidates = []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"}

// DiscoverSystemResolvers returns the system's upstream nameservers as "ip:port", minus any on
// selfAddr's IP (port ignored): forwarding to ourselves would loop.
func DiscoverSystemResolvers(selfAddr string) ([]string, error) {
	return discoverFromFirst(resolvConfCandidates, selfAddr)
}

// DiscoverFromConfig returns the nameservers of the resolv.conf at path as DiscoverSystemResolvers
// does.
func DiscoverFromConfig(path, selfAddr string) ([]string, error) {
	cfg, err := mdns.ClientConfigFromFile(path)
	if err != nil {
		return nil, err
	}

	selfIP, _, _ := net.SplitHostPort(selfAddr)

	out := make([]string, 0, len(cfg.Servers))
	for _, s := range cfg.Servers {
		if s == selfIP {
			continue
		}
		out = append(out, net.JoinHostPort(s, cfg.Port))
	}
	return out, nil
}

func discoverFromFirst(paths []string, selfAddr string) ([]string, error) {
	for _, p := range paths[:len(paths)-1] {
		if _, err := os.Stat(p); err == nil {
			return DiscoverFromConfig(p, selfAddr)
		}
	}
	return DiscoverFromConfig(paths[len(paths)-1], selfAddr)
}
