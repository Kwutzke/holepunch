package resolver

import (
	"net"

	mdns "github.com/miekg/dns"
)

const defaultResolvConf = "/etc/resolv.conf"

// DiscoverSystemResolvers returns the resolv.conf nameservers as "ip:port", minus any on
// selfAddr's IP (port ignored): forwarding to ourselves would loop.
func DiscoverSystemResolvers(selfAddr string) ([]string, error) {
	return discoverFromConfig(defaultResolvConf, selfAddr)
}

func discoverFromConfig(path, selfAddr string) ([]string, error) {
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
