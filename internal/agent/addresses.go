package agent

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"net"
	"strconv"
	"strings"
)

// Flags of the kernel's route tables.
const (
	routeUp     = 0x0001
	routeReject = 0x0200
)

// Flags of an IPv6 address in if_inet6 that make it a poor choice to show.
const (
	inet6Temporary  = 0x01
	inet6DADFailed  = 0x08
	inet6Deprecated = 0x20
	inet6Tentative  = 0x40
)

// containerInterfacePrefixes name the interfaces that Docker and other container
// runtimes create. Their addresses are not how anyone reaches the host.
var containerInterfacePrefixes = []string{
	"docker", "br-", "veth", "cni", "flannel", "cali", "vxlan", "kube", "virbr", "lxcbr", "lxdbr", "podman",
}

func isContainerInterface(name string) bool {
	for _, prefix := range containerInterfacePrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// addresses returns the host's primary IPv4 and IPv6 addresses. They come from the
// host's network namespace, read through its first process: the agent's container
// sits on a bridge network, so its own view, and the address it sends from, only
// show the container's address. The interface of the default route is the primary
// one. Where there is no /proc to read, such as an agent on a development machine,
// the address used for outgoing traffic is the next best answer.
func (h *hostInfo) addresses() (ip4, ip6 string) {
	route, routeErr := h.procOfInit("net/route")
	trie, trieErr := h.procOfInit("net/fib_trie")
	if routeErr == nil && trieErr == nil {
		ip4 = primaryIPv4(string(route), string(trie))
	}

	inet6, inet6Err := h.procOfInit("net/if_inet6")
	if inet6Err == nil {
		// without a default route, any global address will do
		routes6, _ := h.procOfInit("net/ipv6_route")
		ip6 = primaryIPv6(string(inet6), string(routes6))
	}

	if routeErr != nil || trieErr != nil || inet6Err != nil {
		dialed4, dialed6 := outgoingAddresses()
		if routeErr != nil || trieErr != nil {
			ip4 = dialed4
		}
		if inet6Err != nil {
			ip6 = dialed6
		}
	}

	return ip4, ip6
}

// outgoingAddresses returns the addresses used for outgoing traffic. No packet is
// sent: connecting a UDP socket only makes the kernel pick a source address.
func outgoingAddresses() (ip4, ip6 string) {
	if conn, err := net.Dial("udp4", "192.0.2.1:9"); err == nil {
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			ip4 = addr.IP.String()
		}
		conn.Close()
	}
	if conn, err := net.Dial("udp6", "[2001:db8::1]:9"); err == nil {
		if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			ip6 = addr.IP.String()
		}
		conn.Close()
	}
	return ip4, ip6
}

type ipv4Route struct {
	iface  string
	dest   net.IP
	mask   net.IPMask
	metric int
}

// parseIPv4Routes reads /proc/net/route. Its addresses are 32-bit numbers printed
// in the machine's byte order, which is little endian on every supported platform.
func parseIPv4Routes(content string) []ipv4Route {
	routes := []ipv4Route{}

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 8 || fields[0] == "Iface" {
			continue
		}

		flags, errFlags := strconv.ParseUint(fields[3], 16, 32)
		metric, errMetric := strconv.Atoi(fields[6])
		dest, okDest := littleEndianIPv4(fields[1])
		mask, okMask := littleEndianIPv4(fields[7])
		if errFlags != nil || errMetric != nil || !okDest || !okMask || flags&routeUp == 0 || flags&routeReject != 0 {
			continue
		}

		routes = append(routes, ipv4Route{iface: fields[0], dest: dest, mask: net.IPMask(mask.To4()), metric: metric})
	}

	return routes
}

func littleEndianIPv4(text string) (net.IP, bool) {
	value, err := strconv.ParseUint(text, 16, 32)
	if err != nil {
		return nil, false
	}
	ip := make(net.IP, 4)
	binary.LittleEndian.PutUint32(ip, uint32(value))
	return ip, true
}

// localIPv4Addresses lists the addresses of the host itself from /proc/net/fib_trie:
// the entries marked "/32 host LOCAL", in their order, without loopback.
func localIPv4Addresses(trie string) []net.IP {
	addresses := []net.IP{}
	seen := map[string]bool{}

	var current net.IP
	scanner := bufio.NewScanner(strings.NewReader(trie))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if address, found := strings.CutPrefix(line, "|-- "); found {
			current = net.ParseIP(address).To4()
			continue
		}
		if strings.HasPrefix(line, "+--") || strings.HasSuffix(line, ":") {
			current = nil
			continue
		}

		if current != nil && line == "/32 host LOCAL" && !current.IsLoopback() && !seen[current.String()] {
			seen[current.String()] = true
			addresses = append(addresses, current)
		}
	}

	return addresses
}

// primaryIPv4 picks the host's address on the interface of the default route. A
// host whose address is not inside the subnet of any route, such as a VPS with a
// /32 address and an on-link gateway, gets its first address that does not belong
// to a container network.
func primaryIPv4(routeTable, trie string) string {
	routes := parseIPv4Routes(routeTable)

	defaultIface := ""
	bestMetric := 0
	for _, route := range routes {
		ones, _ := route.mask.Size()
		if ones == 0 && route.dest.Equal(net.IPv4zero) && (defaultIface == "" || route.metric < bestMetric) {
			defaultIface, bestMetric = route.iface, route.metric
		}
	}

	interfaceOf := func(ip net.IP) string {
		best, bestOnes := "", -1
		for _, route := range routes {
			ones, _ := route.mask.Size()
			if ones > 0 && ones > bestOnes && route.dest.Mask(route.mask).Equal(ip.Mask(route.mask)) {
				best, bestOnes = route.iface, ones
			}
		}
		return best
	}

	fallback := ""
	for _, address := range localIPv4Addresses(trie) {
		iface := interfaceOf(address)
		if isContainerInterface(iface) {
			continue
		}
		if defaultIface != "" && iface == defaultIface {
			return address.String()
		}
		if fallback == "" {
			fallback = address.String()
		}
	}

	return fallback
}

// primaryIPv6 picks a global IPv6 address of the host from /proc/net/if_inet6,
// on the interface of the default route in /proc/net/ipv6_route if there is one.
func primaryIPv6(inet6, routes string) string {
	defaultIface := ""
	scanner := bufio.NewScanner(strings.NewReader(routes))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 || strings.Trim(fields[0], "0") != "" || fields[1] != "00" || fields[9] == "lo" {
			continue
		}
		flags, err := strconv.ParseUint(fields[8], 16, 32)
		if err != nil || flags&routeUp == 0 || flags&routeReject != 0 {
			continue
		}
		defaultIface = fields[9]
		break
	}

	fallback := ""
	scanner = bufio.NewScanner(strings.NewReader(inet6))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 {
			continue
		}

		raw, err := hex.DecodeString(fields[0])
		scope, errScope := strconv.ParseUint(fields[3], 16, 8)
		flags, errFlags := strconv.ParseUint(fields[4], 16, 8)
		if err != nil || len(raw) != net.IPv6len || errScope != nil || errFlags != nil {
			continue
		}

		iface := fields[5]
		bad := inet6Temporary | inet6DADFailed | inet6Deprecated | inet6Tentative
		if scope != 0 || flags&uint64(bad) != 0 || isContainerInterface(iface) {
			continue
		}

		address := net.IP(raw).String()
		if defaultIface != "" && iface == defaultIface {
			return address
		}
		if fallback == "" {
			fallback = address
		}
	}

	return fallback
}
