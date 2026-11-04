package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// fibTrie is /proc/net/fib_trie of a VPS with Docker: a /32 address with an on-link
// gateway, and the docker0 bridge.
const fibTrie = `Main:
  +-- 0.0.0.0/0 3 0 5
     |-- 0.0.0.0
        /0 universe UNICAST
     +-- 127.0.0.0/8 2 0 2
        +-- 127.0.0.0/31 1 0 0
           |-- 127.0.0.0
              /8 host LOCAL
           |-- 127.0.0.1
              /32 host LOCAL
        |-- 127.255.255.255
           /32 link BROADCAST
     +-- 172.16.0.0/12 2 0 2
        +-- 172.17.0.0/16 2 0 2
           |-- 172.17.0.0
              /16 link UNICAST
           |-- 172.17.0.1
              /32 host LOCAL
           |-- 172.17.255.255
              /32 link BROADCAST
        |-- 172.31.1.1
           /32 link UNICAST
     |-- 203.0.113.24
        /32 host LOCAL
Local:
  +-- 0.0.0.0/0 3 0 5
     |-- 172.17.0.1
        /32 host LOCAL
     |-- 203.0.113.24
        /32 host LOCAL
`

// vpsRoutes is the route table that goes with it: the default route and the host
// route of the gateway on eth0, no subnet that holds the address itself.
const vpsRoutes = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	00000000	01011FAC	0003	0	0	100	00000000	0	0	0
eth0	01011FAC	00000000	0005	0	0	100	FFFFFFFF	0	0	0
docker0	000011AC	00000000	0001	0	0	0	0000FFFF	0	0	0
`

// lanTrie and lanRoutes are a machine at home: a private network on eth1 that
// comes first by address, the default route on eth0, and two Docker bridges.
const lanTrie = `Main:
  +-- 0.0.0.0/0 3 0 5
     |-- 10.0.0.5
        /32 host LOCAL
     |-- 172.17.0.1
        /32 host LOCAL
     |-- 172.18.0.1
        /32 host LOCAL
     |-- 192.168.1.10
        /32 host LOCAL
`

const lanRoutes = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	00000000	0101A8C0	0003	0	0	100	00000000	0	0	0
eth1	0000000A	00000000	0001	0	0	0	00FFFFFF	0	0	0
eth0	0001A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
docker0	000011AC	00000000	0001	0	0	0	0000FFFF	0	0	0
br-1a2b3c4d	000012AC	00000000	0001	0	0	0	0000FFFF	0	0	0
`

// ifInet6 has loopback, a temporary address before the stable one, a link-local
// address and Docker's own network.
const ifInet6 = `00000000000000000000000000000001 01 80 10 80       lo
20010db8000000000000000000000099 02 40 00 01     eth0
20010db8000000000000000000000024 02 40 00 80     eth0
fe80000000000000000000fffe000024 02 40 20 80     eth0
fd000000000000000000000000000001 04 40 00 80  docker0
`

const ipv6Routes = `00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003     eth0
00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200       lo
`

func TestPrimaryIPv4(t *testing.T) {
	if got := primaryIPv4(vpsRoutes, fibTrie); got != "203.0.113.24" {
		t.Errorf("VPS: %q", got)
	}
	if got := primaryIPv4(lanRoutes, lanTrie); got != "192.168.1.10" {
		t.Errorf("LAN: %q", got)
	}

	// without a default route, the first address outside the container networks
	noDefault := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"docker0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"
	if got := primaryIPv4(noDefault, lanTrie); got != "10.0.0.5" {
		t.Errorf("no default route: %q", got)
	}

	if got := primaryIPv4("", ""); got != "" {
		t.Errorf("nothing to read: %q", got)
	}
}

func TestPrimaryIPv6(t *testing.T) {
	if got := primaryIPv6(ifInet6, ipv6Routes); got != "2001:db8::24" {
		t.Errorf("with a default route: %q", got)
	}
	if got := primaryIPv6(ifInet6, ""); got != "2001:db8::24" {
		t.Errorf("without one: %q", got)
	}

	onlyDocker := "fd000000000000000000000000000001 04 40 00 80  docker0\n"
	if got := primaryIPv6(onlyDocker, ""); got != "" {
		t.Errorf("only Docker's network: %q", got)
	}
}

// The agent reads the host's network through the host mount, not its own.
func TestAddressesComeFromTheHostMount(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"proc/stat":                "cpu  1 0 1 1 0 0 0 0 0 0\n",
		"proc/1/net/route":         vpsRoutes,
		"proc/1/net/fib_trie":      fibTrie,
		"proc/1/net/if_inet6":      ifInet6,
		"proc/1/net/ipv6_route":    ipv6Routes,
		"proc/net/route":           "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n",
		"proc/net/fib_trie":        "Main:\n     |-- 172.17.0.2\n        /32 host LOCAL\n",
		"proc/sys/kernel/hostname": "container\n",
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ip4, ip6 := newHostInfo(root).addresses()
	if ip4 != "203.0.113.24" || ip6 != "2001:db8::24" {
		t.Errorf("addresses: %q %q", ip4, ip6)
	}
}
