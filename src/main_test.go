package main

import "testing"

func TestDefaultConfigPath(t *testing.T) {
	for _, tc := range []struct{ executable, workdir, want string }{
		{"/usr/bin/dns-server", "/tmp/run", "/etc/calidns/config.yaml"},
		{"/usr/local/bin/dns-server", "/tmp/run", "/etc/calidns/config.yaml"},
		{"/tmp/dns-server", "/tmp/run", "/tmp/run/config.yaml"},
	} {
		if got := defaultConfigPath(tc.executable, tc.workdir); got != tc.want {
			t.Errorf("defaultConfigPath(%q, %q) = %q, want %q", tc.executable, tc.workdir, got, tc.want)
		}
	}
}

func TestNewDNSServers(t *testing.T) {
	servers := newDNSServers([]string{"192.0.2.42:53", "[2001:db8::42]:53"})
	if len(servers) != 4 {
		t.Fatalf("got %d servers, want 4", len(servers))
	}
	for i, address := range []string{"192.0.2.42:53", "[2001:db8::42]:53"} {
		if servers[2*i].Addr != address || servers[2*i].Net != "udp" || servers[2*i+1].Addr != address || servers[2*i+1].Net != "tcp" {
			t.Fatalf("incorrect servers for %s: %+v %+v", address, servers[2*i], servers[2*i+1])
		}
	}
}
