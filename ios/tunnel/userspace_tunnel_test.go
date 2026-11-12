package tunnel

import (
	"context"
	"io"
	"net"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Masterminds/semver"
	"github.com/danielpaulus/go-ios/ios"
)

const userspaceHandshake = `{"ServerAddress":"fd00::1","ServerRSDPort":12345,"ClientParameters":{"Address":"fd00::2","Mtu":1280}}`

// Exercise the real handshake, listener and network-stack setup without a device.
func userspaceTestConnection(t *testing.T, response string, beforeReply func()) net.Conn {
	t.Helper()
	client, peer := net.Pipe()
	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = client.Close()
		_ = peer.Close()
		<-done
	})
	go func() {
		defer close(done)
		defer peer.Close()
		header := make([]byte, 10)
		if _, err := io.ReadFull(peer, header); err != nil {
			return
		}
		if _, err := io.CopyN(io.Discard, peer, int64(header[9])); err != nil {
			return
		}
		if beforeReply != nil {
			beforeReply()
		}
		if _, err := peer.Write(append([]byte("CDTunnel\x00"), append([]byte{byte(len(response))}, []byte(response)...)...)); err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, peer)
	}()
	return client
}

func userspaceTestListener(t *testing.T, port int) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort("localhost", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func TestUserspaceTunnelConcurrentPortsAndIndependentClose(t *testing.T) {
	// Model the host's already-running RPC and tunnel-info listeners.
	rpc := userspaceTestListener(t, 0)
	agent := userspaceTestListener(t, 0)
	occupied := map[int]bool{rpc.Addr().(*net.TCPAddr).Port: true, agent.Addr().(*net.TCPAddr).Port: true}
	type result struct {
		tunnel Tunnel
		err    error
	}
	results := make(chan result, 2)
	for _, udid := range []string{"device-a", "device-b"} {
		conn := userspaceTestConnection(t, userspaceHandshake, nil)
		go func() {
			tun, err := connectToUserspaceTunnelLockdown(context.Background(), devEntry(udid, "USB"), conn, 0)
			results <- result{tun, err}
		}()
	}
	var tunnels []Tunnel
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		t.Cleanup(func() { _ = r.tunnel.Close() })
		port := r.tunnel.UserspaceTUNPort
		if !r.tunnel.UserspaceTUN || port <= 0 || occupied[port] {
			t.Fatalf("invalid or conflicting tunnel port: %+v", r.tunnel)
		}
		occupied[port] = true
		tunnels = append(tunnels, r.tunnel)
		// Existing clients must discover the bound port through the same API.
		server := httptest.NewServer(tunnelInfoMux(tunnelManagerWithTunnels(map[string]Tunnel{r.tunnel.Udid: r.tunnel})))
		t.Cleanup(server.Close)
		host, agentPort := serverHostPort(t, server.URL)
		info, err := TunnelInfoForDevice(r.tunnel.Udid, host, agentPort)
		if err != nil || info.UserspaceTUNPort != port || !info.UserspaceTUN {
			t.Fatalf("tunnel info lost actual port: info=%+v err=%v", info, err)
		}
	}
	if err := tunnels[0].Close(); err != nil {
		t.Fatal(err)
	}
	userspaceTestListener(t, tunnels[0].UserspaceTUNPort)
	for _, port := range []int{tunnels[1].UserspaceTUNPort, rpc.Addr().(*net.TCPAddr).Port, agent.Addr().(*net.TCPAddr).Port} {
		ln, err := net.Listen("tcp", net.JoinHostPort("localhost", strconv.Itoa(port)))
		if err == nil {
			_ = ln.Close()
			t.Fatalf("closing one tunnel released another listener on %d", port)
		}
	}
}

func TestUserspaceTunnelExplicitPortHeldDuringHandshake(t *testing.T) {
	probe := userspaceTestListener(t, 0)
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	handshake := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	resume := func() { once.Do(func() { close(release) }) }
	conn := userspaceTestConnection(t, userspaceHandshake, func() {
		close(handshake)
		<-release
	})
	t.Cleanup(resume)
	type result struct {
		tunnel Tunnel
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		tun, err := connectToUserspaceTunnelLockdown(context.Background(), devEntry("device", "USB"), conn, port)
		resultCh <- result{tun, err}
	}()
	select {
	case <-handshake:
	case <-time.After(5 * time.Second):
		t.Fatal("handshake did not start")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("localhost", strconv.Itoa(port)))
	if err == nil {
		_ = ln.Close()
		t.Error("requested port was not held during handshake")
	}
	resume()
	r := <-resultCh
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { _ = r.tunnel.Close() })
	if r.tunnel.UserspaceTUNPort != port {
		t.Fatalf("explicit port changed: got %d want %d", r.tunnel.UserspaceTUNPort, port)
	}
}

type closeTrackingConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *closeTrackingConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

func TestUserspaceTunnelSetupFailureReleasesResources(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		occupied       bool
	}{
		{"occupied port", userspaceHandshake, true},
		{"invalid handshake", `{`, false},
		{"invalid address", `{"ClientParameters":{"Address":"invalid","Mtu":1280}}`, false},
		{"invalid mtu", `{"ClientParameters":{"Address":"fd00::2","Mtu":0}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := userspaceTestListener(t, 0)
			port := probe.Addr().(*net.TCPAddr).Port
			if !tc.occupied {
				_ = probe.Close()
			}
			conn := &closeTrackingConn{Conn: userspaceTestConnection(t, tc.response, nil)}
			_, err := connectToUserspaceTunnelLockdown(context.Background(), devEntry("device", "USB"), conn, port)
			if err == nil {
				t.Fatal("expected setup failure")
			}
			if !conn.closed.Load() {
				t.Fatal("failed setup left device connection open")
			}
			if tc.occupied {
				_ = probe.Close()
			}
			userspaceTestListener(t, port)
		})
	}
}

func TestTunnelManagerLeavesAutomaticAndExplicitPortsToStarter(t *testing.T) {
	fixed := devEntry("fixed", "USB")
	fixed.UserspaceTUNPort = 12345
	tm := NewTunnelManagerForDevice(PairRecordManager{}, true, "", 65535)
	tm.dl = stubDeviceLister{list: ios.DeviceList{DeviceList: []ios.DeviceEntry{devEntry("automatic", "USB"), fixed}}}
	starter := &recordingTunnelStarter{}
	tm.ts = starter
	tm.getProductVersion = func(ios.DeviceEntry) (*semver.Version, error) {
		return semver.MustParse("18.0.0"), nil
	}
	if err := tm.UpdateTunnels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(starter.calls) != 2 {
		t.Fatalf("got %d starts, want 2", len(starter.calls))
	}
	for _, device := range starter.calls {
		want := 0
		if device.Properties.SerialNumber == "fixed" {
			want = 12345
		}
		if device.UserspaceTUNPort != want {
			t.Fatalf("%s port = %d, want %d", device.Properties.SerialNumber, device.UserspaceTUNPort, want)
		}
	}
}
