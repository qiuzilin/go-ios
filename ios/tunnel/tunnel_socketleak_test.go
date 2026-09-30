package tunnel

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Masterminds/semver"
	"github.com/danielpaulus/go-ios/ios"
)

type stubDeviceLister struct{ list ios.DeviceList }

func (s stubDeviceLister) ListDevices() (ios.DeviceList, error) { return s.list, nil }

func devEntry(udid, connType string) ios.DeviceEntry {
	return ios.DeviceEntry{Properties: ios.DeviceProperties{SerialNumber: udid, ConnectionType: connType}}
}

func leakTestManager(entries ...ios.DeviceEntry) *TunnelManager {
	return &TunnelManager{
		dl:                 stubDeviceLister{list: ios.DeviceList{DeviceList: entries}},
		tunnels:            map[string]Tunnel{},
		failedDevices:      map[string]failedDevice{},
		startTunnelTimeout: time.Second,
	}
}

type recordingTunnelStarter struct {
	calls  []ios.DeviceEntry
	err    error
	closes int
}

func (s *recordingTunnelStarter) StartTunnel(_ context.Context, device ios.DeviceEntry, _ PairRecordManager, _ *semver.Version, _ bool) (Tunnel, error) {
	s.calls = append(s.calls, device)
	if s.err != nil {
		return Tunnel{}, s.err
	}
	return Tunnel{Udid: device.Properties.SerialNumber, closer: func() error { s.closes++; return nil }}, nil
}

func TestFailedDeviceBackoff(t *testing.T) {
	cases := []struct {
		failCount int
		want      time.Duration
	}{
		{0, 30 * time.Second}, // shift<0 guard clamps to 0
		{1, 30 * time.Second},
		{2, 60 * time.Second},
		{3, 120 * time.Second},
		{4, 240 * time.Second},
		{5, 300 * time.Second}, // 480s capped to 5 min
		{10, 300 * time.Second},
	}
	for _, c := range cases {
		if got := failedDeviceBackoff(c.failCount); got != c.want {
			t.Errorf("failedDeviceBackoff(%d) = %v, want %v", c.failCount, got, c.want)
		}
	}
}

func TestShouldSkipDevice(t *testing.T) {
	now := time.Now()
	failed := map[string]failedDevice{
		"recent": {lastAttempt: now.Add(-10 * time.Second), failCount: 1, connectionType: "USB"}, // 30s window, 10s elapsed → skip
		"stale":  {lastAttempt: now.Add(-31 * time.Second), failCount: 1, connectionType: "USB"}, // 30s window, 31s elapsed → retry
	}
	cases := []struct {
		name, udid, conn string
		want             bool
	}{
		{"network device attempted", "net", "Network", false},
		{"fresh usb attempted", "fresh", "USB", false},
		{"recent failure backed off", "recent", "USB", true},
		{"stale failure retried", "stale", "USB", false},
		{"different transport is retried immediately", "recent", "Network", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldSkipDevice(devEntry(c.udid, c.conn), failed, now); got != c.want {
				t.Fatalf("shouldSkipDevice = %v, want %v", got, c.want)
			}
		})
	}
}

func TestUpdateTunnelsRetriesAfterTransportChanges(t *testing.T) {
	tm := leakTestManager(devEntry("device-1", "Network"))
	starter := &recordingTunnelStarter{}
	tm.ts = starter
	tm.getProductVersion = func(ios.DeviceEntry) (*semver.Version, error) {
		return semver.MustParse("17.4.0"), nil
	}
	tm.failedDevices["device-1"] = failedDevice{
		lastAttempt:    time.Now(),
		failCount:      1,
		connectionType: "USB",
	}

	if err := tm.UpdateTunnels(context.Background()); err != nil {
		t.Fatalf("UpdateTunnels: %v", err)
	}
	if len(starter.calls) != 1 || starter.calls[0].Properties.ConnectionType != "Network" {
		t.Fatalf("wireless transport was suppressed by USB failure: calls=%+v", starter.calls)
	}
	if _, ok := tm.failedDevices["device-1"]; ok {
		t.Fatal("successful wireless tunnel should clear prior USB failure")
	}
}

func TestUpdateTunnelsSupportsNetworkAndTransportSwitch(t *testing.T) {
	usb := devEntry("device-1", "USB")
	network := devEntry("device-1", "Network")
	tm := leakTestManager(usb, network)
	starter := &recordingTunnelStarter{}
	tm.ts = starter
	tm.getProductVersion = func(ios.DeviceEntry) (*semver.Version, error) {
		return semver.MustParse("17.4.0"), nil
	}

	if err := tm.UpdateTunnels(context.Background()); err != nil {
		t.Fatalf("UpdateTunnels with USB and Network entries: %v", err)
	}
	if len(starter.calls) != 1 || starter.calls[0].Properties.ConnectionType != "USB" {
		t.Fatalf("expected one USB tunnel attempt when both transports are present, calls=%+v", starter.calls)
	}
	if got := tm.tunnels["device-1"].ConnectionType; got != "USB" {
		t.Fatalf("tunnel connection type = %q, want USB", got)
	}

	tm.dl = stubDeviceLister{list: ios.DeviceList{DeviceList: []ios.DeviceEntry{network}}}
	if err := tm.UpdateTunnels(context.Background()); err != nil {
		t.Fatalf("UpdateTunnels after USB disconnect: %v", err)
	}
	if starter.closes != 1 {
		t.Fatalf("stale USB tunnel close count = %d, want 1", starter.closes)
	}
	if len(starter.calls) != 2 || starter.calls[1].Properties.ConnectionType != "Network" {
		t.Fatalf("expected wireless tunnel after USB disconnect, calls=%+v", starter.calls)
	}
	if got := tm.tunnels["device-1"].ConnectionType; got != "Network" {
		t.Fatalf("tunnel connection type = %q, want Network", got)
	}
	if err := tm.UpdateTunnels(context.Background()); err != nil {
		t.Fatalf("repeated UpdateTunnels: %v", err)
	}
	if len(starter.calls) != 2 {
		t.Fatalf("repeated update started a duplicate tunnel: calls=%+v", starter.calls)
	}
}

func TestNetworkTunnelFailureUsesBackoff(t *testing.T) {
	tm := leakTestManager(devEntry("net-1", "Network"))
	starter := &recordingTunnelStarter{err: errors.New("connection refused")}
	tm.ts = starter
	tm.getProductVersion = func(ios.DeviceEntry) (*semver.Version, error) {
		return semver.MustParse("17.4.0"), nil
	}
	for range 2 {
		if err := tm.UpdateTunnels(context.Background()); err != nil {
			t.Fatalf("UpdateTunnels: %v", err)
		}
	}
	if len(starter.calls) != 1 {
		t.Fatalf("network failure was retried inside its backoff window: attempts=%d", len(starter.calls))
	}
	if got := tm.failedDevices["net-1"].failCount; got != 1 {
		t.Fatalf("network failure count = %d, want 1", got)
	}
}

func TestUpdateTunnelsRestartsClosedNetworkTunnel(t *testing.T) {
	tm := leakTestManager(devEntry("net-1", "Network"))
	starter := &recordingTunnelStarter{}
	tm.ts = starter
	tm.getProductVersion = func(ios.DeviceEntry) (*semver.Version, error) {
		return semver.MustParse("17.4.0"), nil
	}
	closed := make(chan struct{})
	tm.tunnels["net-1"] = Tunnel{
		Udid:           "net-1",
		ConnectionType: "Network",
		closed:         closed,
		closer:         func() error { starter.closes++; return nil },
	}
	if err := tm.UpdateTunnels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(starter.calls) != 0 {
		t.Fatal("live network tunnel was restarted")
	}
	close(closed)
	if err := tm.UpdateTunnels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if starter.closes != 1 || len(starter.calls) != 1 {
		t.Fatalf("closed network tunnel was not replaced: closes=%d starts=%d", starter.closes, len(starter.calls))
	}
	if err := tm.UpdateTunnels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(starter.calls) != 1 {
		t.Fatal("replacement network tunnel was restarted again")
	}
}

// A device still inside its backoff window must not be retried — the leak this
// whole change prevents. Pre-seeding a long backoff means UpdateTunnels skips it
// and leaves its entry untouched (no new attempt timestamp).
func TestUpdateTunnelsRespectsBackoff(t *testing.T) {
	tm := leakTestManager(devEntry("usb-1", "USB"))
	seeded := time.Now()
	tm.failedDevices["usb-1"] = failedDevice{lastAttempt: seeded, failCount: 5} // 5 min backoff
	if err := tm.UpdateTunnels(context.Background()); err != nil {
		t.Fatalf("UpdateTunnels: %v", err)
	}
	got, ok := tm.failedDevices["usb-1"]
	if !ok {
		t.Fatal("usb-1 should remain in failedDevices (still backed off)")
	}
	if !got.lastAttempt.Equal(seeded) || got.failCount != 5 {
		t.Fatalf("backed-off device must not be retried; entry changed: %+v", got)
	}
}

// failedDevices entries for devices that are no longer connected get pruned, so
// a reconnect retries immediately instead of waiting out a stale backoff.
func TestUpdateTunnelsPrunesDisconnectedFailedDevices(t *testing.T) {
	tm := leakTestManager(devEntry("net-1", "Network"))
	tm.failedDevices["net-1"] = failedDevice{lastAttempt: time.Now(), failCount: 5}
	tm.failedDevices["gone"] = failedDevice{lastAttempt: time.Now(), failCount: 2}
	if err := tm.UpdateTunnels(context.Background()); err != nil {
		t.Fatalf("UpdateTunnels: %v", err)
	}
	if _, ok := tm.failedDevices["gone"]; ok {
		t.Fatal("disconnected device should be pruned from failedDevices")
	}
}

// A USB device whose tunnel start fails (no real backend on CI → GetProductVersion
// errors) must be recorded so the next cycle backs off instead of retrying.
func TestUpdateTunnelsRecordsFailure(t *testing.T) {
	tm := leakTestManager(devEntry("usb-1", "USB"))
	if err := tm.UpdateTunnels(context.Background()); err != nil {
		t.Fatalf("UpdateTunnels: %v", err)
	}
	got, ok := tm.failedDevices["usb-1"]
	if !ok || got.failCount != 1 {
		t.Fatalf("failed USB device should be recorded with failCount=1, got ok=%v entry=%+v", ok, got)
	}
}
