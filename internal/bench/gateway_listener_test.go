package bench

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestGatewayListenerAddressFamily(t *testing.T) {
	for _, tc := range []struct {
		name, host, connectHost string
		ipv6, autoFamily        bool
	}{
		{name: "default", connectHost: "127.0.0.1"},
		{name: "IPv4 loopback", host: "127.0.0.1", connectHost: "127.0.0.1"},
		{name: "IPv4 wildcard", host: "0.0.0.0", connectHost: "127.0.0.1"},
		{name: "IPv4-mapped loopback", host: "::ffff:127.0.0.1", connectHost: "127.0.0.1"},
		{name: "hostname", host: "localhost", connectHost: "localhost", autoFamily: true},
		{name: "IPv6 loopback", host: "::1", connectHost: "::1", ipv6: true},
		{name: "IPv6 wildcard", host: "::", connectHost: "::1", ipv6: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.ipv6 {
				probe, err := net.Listen("tcp6", "[::1]:0")
				if err != nil {
					t.Skipf("IPv6 loopback is unavailable: %v", err)
				}
				_ = probe.Close()
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"data":[]}`)
			}))
			defer upstream.Close()
			gateway := gatewayForTest(t, "local-fake-key", upstream.URL, GatewayOptions{Host: tc.host})
			address, ok := gateway.listener.Addr().(*net.TCPAddr)
			if !ok {
				t.Fatalf("listener address is not TCP: %T", gateway.listener.Addr())
			}
			if gotIPv6 := address.IP.To4() == nil; !tc.autoFamily && gotIPv6 != tc.ipv6 {
				t.Fatalf("Host %q bound %s; want IPv6=%t", tc.host, address, tc.ipv6)
			}
			if !strings.HasSuffix(gateway.ContainerURL(), fmt.Sprintf(":%d/v1", address.Port)) {
				t.Fatalf("container URL lost assigned port: %s", gateway.ContainerURL())
			}
			transport := &http.Transport{Proxy: nil}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
			endpoint := "http://" + net.JoinHostPort(tc.connectHost, fmt.Sprint(address.Port)) + "/v1/models"
			request, _ := http.NewRequest(http.MethodGet, endpoint, nil)
			request.Header = gatewayAuth(gateway)
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || string(body) != `{"data":[]}` {
				t.Fatalf("discovery status=%d body=%q error=%v", response.StatusCode, body, err)
			}
		})
	}
}

func TestIndependentGatewayIPv4WildcardNotIPv6(t *testing.T) {
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 unavailable: %v", err)
	}
	_ = probe.Close()
	gateway, err := NewGateway("local-only-dummy-key", GatewayOptions{Host: "0.0.0.0", Upstream: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Close()
	endpoint, _ := url.Parse(gateway.BaseURL())
	connection, err := net.DialTimeout("tcp6", net.JoinHostPort("::1", endpoint.Port()), time.Second)
	if err == nil {
		_ = connection.Close()
		t.Fatal("explicit IPv4 wildcard unexpectedly accepts IPv6 connections")
	}
}
