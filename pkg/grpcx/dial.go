// Package grpcx provides small gRPC dialing helpers shared by MonoFS and
// Guardian.
package grpcx

import (
	"context"
	"net"
	"time"

	"google.golang.org/grpc"
)

// IPv4DialerOption returns a gRPC DialOption that ensures connections are made
// over IPv4.
//
// ECS Service Connect advertises both an IPv4 VIP (127.255.0.0/16) and an IPv6
// VIP (2600:f0f0::/32) for each service. In IPv4-only VPCs, dialing the IPv6
// VIP fails immediately with "network is unreachable". gRPC's default "dns"
// resolver resolves the hostname before the dialer runs, so by the time the
// dialer is invoked the address is already an IPv6 literal. This option maps
// the Service Connect IPv6 VIP back to the corresponding IPv4 VIP and resolves
// hostnames to IPv4 only.
func IPv4DialerOption() grpc.DialOption {
	return grpc.WithContextDialer(IPv4DialContext)
}

// IPv4DialContext dials addr over TCP, forcing IPv4.
func IPv4DialContext(ctx context.Context, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err == nil {
		if ip := net.ParseIP(host); ip != nil {
			if v4 := serviceConnectIPv4(ip); v4 != nil {
				addr = net.JoinHostPort(v4.String(), port)
			}
		} else if ips, lookupErr := net.DefaultResolver.LookupIP(ctx, "ip4", host); lookupErr == nil && len(ips) > 0 {
			addr = net.JoinHostPort(ips[0].String(), port)
		}
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	return dialer.DialContext(ctx, "tcp", addr)
}

// serviceConnectIPv4 maps an ECS Service Connect IPv6 VIP (2600:f0f0::/32) to
// its IPv4 counterpart (127.255.0.0/16). The low 16 bits identify the service
// endpoint in both ranges. Returns nil for any other address.
func serviceConnectIPv4(ip net.IP) net.IP {
	if ip.To4() != nil {
		return nil
	}
	v6 := ip.To16()
	if v6 == nil {
		return nil
	}
	if v6[0] != 0x26 || v6[1] != 0x00 || v6[2] != 0xf0 || v6[3] != 0xf0 {
		return nil
	}
	low := int(v6[14])<<8 | int(v6[15])
	return net.IPv4(127, 255, byte(low>>8), byte(low))
}
