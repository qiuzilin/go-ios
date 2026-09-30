# Wireless CoreDeviceProxy probe

An isolated experiment on branch `codex/wireless-probe`, based on
`feature/dtx-int64-arguments` at `7c7f781`.

The probe explicitly selects a usbmux `Network` entry. It refuses a target
that also has a `USB` entry, opens a userspace CoreDeviceProxy tunnel directly,
verifies the RSD device identity, and receives advancing sysmontap CPU samples.
It checks the target is still wireless-only at completion. It does not start
the tunnel agent, modify TunnelManager policy, pair devices, enable wireless
connections, mount developer images, or launch apps.

## Run

Use an already-paired device on the same reachable network. Prepare Developer
Mode and the developer image before disconnecting USB. This probe requires
iOS 17.4 or later; it does not test the older RemotePairing path.

From the repository root:

```sh
go build -o /tmp/go-ios-wireless-probe ./cmd/wireless-probe
/tmp/go-ios-wireless-probe
/tmp/go-ios-wireless-probe --udid <UDID> --samples 20 --timeout 45s
```

The first invocation lists device IDs and connection types without connecting
to any device service. The second emits NDJSON stage results and CPU samples
to stdout; existing go-ios diagnostics go to stderr. Use `--port` if the local
default port 61235 is occupied. The overall timeout exits the process to bound
existing library calls that do not accept deadlines and release their sockets.

Success means exit code 0 and an `event: pass` record. `totalLoad` is the raw
system CPU load summed across cores, not a normalized 0-100 percentage. The
number of cores is provided separately as `cpuCount`. CoreDeviceProxy may
publish other services even when Instruments is unavailable, so tunnel success
alone does not imply performance collection is ready.

## Observed Results (2026-09-30)

Host: macOS 14.8.7. All targets were already paired and appeared only as
`Network` entries throughout their probes. No device preparation was performed
by the probe.

| Target | Wireless lockdown | Userspace tunnel | RSD | CPU stream |
| --- | --- | --- | --- | --- |
| iOS 18.5.0, UDID ending `2EE8802E` | PASS | PASS | PASS, 70 services, Instruments present | PASS: 5 samples, then 20 samples on a separate connection |
| iOS 26.6.2, UDID ending `3C87801C` | PASS | PASS | PASS, 59 services | Not collected: Instruments absent from RSD |
| iOS 26.6.1, UDID ending `3E00001E` | PASS | PASS | PASS, 73 services, Instruments present | PASS: 20 samples, no USB entry at start or completion |
| iOS 27.0.0, UDID ending `3C92001E` | PASS | PASS | PASS, 76 services, Instruments present | PASS: 20 samples, no USB entry at start or completion |

The second successful iOS 18.5 run returned 20 samples over approximately
10 seconds, with 6 CPUs and strictly advancing `EndMachAbsTime` values. Example:

```json
{"cpuCount":6,"event":"cpu_sample","index":2,"machTime":848295842309,"totalLoad":81.29411764705881}
{"cpuCount":6,"event":"cpu_sample","index":20,"machTime":848514322888,"totalLoad":81.25490196078432}
{"connection":"Network","event":"pass","samples":20,"udid":"00008030-001509C92EE8802E"}
```

The user-selected target `00008101-00067CA83E00001E` initially had both USB
and Network entries. After the user disconnected USB, the probe selected its
Network entry (device ID 362), read iOS 26.6.1, and received 20 samples with
6 CPUs and strictly advancing timestamps. The full probe completed in
approximately 14 seconds. Its last sample and result were:

```json
{"cpuCount":6,"event":"cpu_sample","index":20,"machTime":8149552666479,"totalLoad":148.94117647058823}
{"connection":"Network","event":"pass","samples":20,"udid":"00008101-00067CA83E00001E"}
```

The next user-selected target `00008101-000E5DD43C92001E` also initially
had both USB and Network entries. Once USB disappeared, the probe selected
Network device ID 474, read iOS 27.0.0, established the tunnel, and discovered
76 RSD services including Instruments. It received 20 samples with 6 CPUs and
strictly advancing timestamps, rechecked that no USB entry existed, and exited
with code 0. Its last sample and result were:

```json
{"cpuCount":6,"event":"cpu_sample","index":20,"machTime":8723275113718,"totalLoad":180.63465386154462}
{"connection":"Network","event":"pass","samples":20,"udid":"00008101-000E5DD43C92001E"}
```

## TunnelManager Integration (2026-09-30)

After enabling Network entries in `TunnelManager`, the go-ios agent from this
branch was started for `00008101-000E5DD43C92001E` with userspace networking and
a dedicated HTTP port. `ios tunnel ls` reported:

```json
[{"address":"fd64:2c01:c54d::1","rsdPort":51731,"udid":"00008101-000E5DD43C92001E","connectionType":"Network","userspaceTun":true,"userspaceTunPort":28178}]
```

Then `ios sysmontap` connected through that agent and streamed CPU samples with
advancing timestamps for about 15 seconds. The test agent was stopped and its
HTTP endpoint returned connection refused afterwards, confirming shutdown.

This establishes short wireless CPU collection using the existing bottom-level
go-ios APIs. It does not establish automatic TunnelManager support, FPS/memory/
GPU collection, long-running stability, reconnection, transport switching, or
compatibility across all devices and host operating systems. Missing Instruments
on the iOS 26.6.2 target requires further device preparation diagnosis; this run
does not establish the cause or demonstrate a wireless protocol limitation.

Existing tunnel teardown code logs some closed-connection errors after an
intentional close. All successful probes exited with code 0 after cleanup.

## Checks

```sh
go build ./...
go test -timeout 60s ./cmd/wireless-probe ./ios/tunnel ./ios/instruments
go test -timeout 60s -skip '^(TestUsesProxy|TestWorksWithoutProxy)$' ./...
git diff --check
```

An initial unfiltered `go test ./...` run was stopped while the imagemounter
package was running. Its existing `TestUsesProxy` and `TestWorksWithoutProxy`
download real images and can mount them on the first connected device. Those
two tests are excluded from the bounded repository check above.
