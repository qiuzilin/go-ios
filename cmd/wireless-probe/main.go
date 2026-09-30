// wireless-probe checks an already-paired, USB-disconnected device without
// changing the TunnelManager's network-device policy or device settings.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Masterminds/semver"
	"github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/instruments"
	"github.com/danielpaulus/go-ios/ios/tunnel"
)

func main() {
	udid := flag.String("udid", "", "UDID of an already-paired wireless device (empty lists devices)")
	samples := flag.Int("samples", 5, "number of CPU samples required")
	port := flag.Int("port", 61235, "local userspace tunnel port")
	timeout := flag.Duration("timeout", 45*time.Second, "overall probe timeout")
	flag.Parse()
	if *samples < 2 || *port < 1 || *port > 65535 || *timeout <= 0 {
		fmt.Fprintln(os.Stderr, "samples must be >= 2, port must be 1..65535, timeout must be positive")
		os.Exit(2)
	}
	result := make(chan error, 1)
	go func() { result <- probe(*udid, *samples, *port) }()
	timer := time.NewTimer(*timeout)
	defer timer.Stop()
	select {
	case err := <-result:
		if err != nil {
			fmt.Fprintln(os.Stderr, "FAIL:", err)
			os.Exit(1)
		}
	case <-timer.C:
		// Some existing service calls have no deadline. Process exit also
		// releases their sockets, bounding even a stalled tunnel handshake.
		fmt.Fprintln(os.Stderr, "FAIL: probe timed out after", *timeout)
		os.Exit(1)
	}
}

func emit(event string, fields map[string]any) {
	fields["event"] = event
	_ = json.NewEncoder(os.Stdout).Encode(fields)
}

func networkDevice(udid string) (ios.DeviceEntry, error) {
	list, err := ios.ListDevices()
	if err != nil {
		return ios.DeviceEntry{}, err
	}
	var network ios.DeviceEntry
	for _, d := range list.DeviceList {
		if d.Properties.SerialNumber != udid {
			continue
		}
		if d.Properties.ConnectionType == "USB" {
			return ios.DeviceEntry{}, fmt.Errorf("device %s still has a USB connection; disconnect it before probing", udid)
		}
		if d.Properties.ConnectionType == "Network" {
			network = d
		}
	}
	if network.Properties.SerialNumber == "" {
		return ios.DeviceEntry{}, fmt.Errorf("no Network entry for %s", udid)
	}
	return network, nil
}

func probe(udid string, samples, port int) error {
	if udid == "" {
		list, err := ios.ListDevices()
		if err != nil {
			return err
		}
		for _, d := range list.DeviceList {
			emit("device", map[string]any{"udid": d.Properties.SerialNumber, "connection": d.Properties.ConnectionType, "deviceID": d.DeviceID})
		}
		return nil
	}
	d, err := networkDevice(udid)
	if err != nil {
		return err
	}
	emit("network_selected", map[string]any{"udid": udid, "deviceID": d.DeviceID})
	v, err := ios.GetProductVersion(d)
	if err != nil {
		return fmt.Errorf("wireless lockdown: %w", err)
	}
	emit("lockdown_ok", map[string]any{"version": v.String()})
	if v.LessThan(semver.MustParse("17.4.0")) {
		return fmt.Errorf("this probe requires iOS >= 17.4, got %s", v)
	}
	emit("tunnel_start", map[string]any{"port": port})
	tun, err := tunnel.ConnectUserSpaceTunnelLockdown(d, port)
	if err != nil {
		return fmt.Errorf("wireless CoreDeviceProxy tunnel: %w", err)
	}
	defer tun.Close()
	d.Address = tun.Address
	d.UserspaceTUN = true
	d.UserspaceTUNHost = "127.0.0.1"
	d.UserspaceTUNPort = port
	emit("tunnel_ok", map[string]any{"address": tun.Address, "rsdPort": tun.RsdPort})
	rsd, err := ios.NewWithAddrPortDevice(tun.Address, tun.RsdPort, d)
	if err != nil {
		return fmt.Errorf("wireless RSD connection: %w", err)
	}
	defer rsd.Close()
	handshake, err := rsd.Handshake()
	if err != nil {
		return fmt.Errorf("wireless RSD handshake: %w", err)
	}
	if handshake.Udid != udid {
		return fmt.Errorf("RSD device mismatch: expected %s, got %s", udid, handshake.Udid)
	}
	d.Rsd = handshake
	servicePort := handshake.GetPort("com.apple.instruments.dtservicehub")
	emit("rsd_ok", map[string]any{"udid": handshake.Udid, "services": len(handshake.Services), "instrumentsPort": servicePort})
	if servicePort == 0 {
		return fmt.Errorf("Instruments is not advertised; prepare the developer image over USB before this probe")
	}
	svc, err := instruments.NewSysmontapService(d, 10)
	if err != nil {
		return fmt.Errorf("wireless sysmontap: %w", err)
	}
	defer svc.Close()
	stream := svc.ReceiveCPUUsage()
	var previous uint64
	for i := 0; i < samples; i++ {
		sample, ok := <-stream
		if !ok {
			return fmt.Errorf("CPU stream ended after %d samples", i)
		}
		if sample.CPUCount == 0 || (i > 0 && sample.EndMachAbsTime <= previous) {
			return fmt.Errorf("invalid or non-advancing CPU sample: %+v", sample)
		}
		previous = sample.EndMachAbsTime
		emit("cpu_sample", map[string]any{"index": i + 1, "cpuCount": sample.CPUCount, "totalLoad": sample.SystemCPUUsage.CPU_TotalLoad, "machTime": sample.EndMachAbsTime})
	}
	if _, err := networkDevice(udid); err != nil {
		return fmt.Errorf("final wireless-only check: %w", err)
	}
	emit("pass", map[string]any{"udid": udid, "samples": samples, "connection": "Network"})
	return nil
}
