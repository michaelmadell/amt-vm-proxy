# fake-amt

Pretends to be an Intel AMT (vPro) device for [Device Management Toolkit Console](https://github.com/device-management-toolkit/console),
and bridges Console's **KVM** sessions to an ordinary VNC server — typically a QEMU/libvirt VM.

That lets you drive Console's real KVM relay and the real browser KVM client (ui-toolkit) against a
live remote desktop with no vPro hardware, e.g. on AMD machines.

```
browser (sample-web-ui / ui-toolkit)
   │  websocket /relay/webrelay.ashx
Console  ──────────── WS-Management :16992 ───┐
   │  AMT redirection :16994                   │
   ▼                                           ▼
fake-amt  ── recorded AMT responses (power, features, hardware info…)
   │  RFB
   ▼
VNC server  (QEMU -vnc, libvirt, …)  ──  the VM you actually look at
```

## What is real and what is emulated

| Part | Status |
|---|---|
| Console's REST API, KVM relay, auth interception | **Real** — your Console, unmodified |
| Browser KVM client (ui-toolkit decode, mouse, keyboard) | **Real** |
| AMT redirection handshake (StartRedirectionSession, digest auth, KVM start) | Emulated to match what Console and ui-toolkit expect |
| KVM picture / input | Real VM desktop, **re-encoded the way AMT sends it**: RFB 3.8, 64×64 RAW tiles, RGB565 or RGB332 |
| WS-Management (features, power state, hardware info…) | Canned: 295 responses recorded from real AMT devices (from go-wsman-messages' test fixtures). Changes (power actions, enabling features) return success but nothing changes |
| IDER (virtual media) | Emulated: fake-amt plays AMT *and* the booting host, reading the mounted image over the real IDER protocol. With `-hyperv-vm` the image is also inserted into a Hyper-V VM's DVD drive. Read-only (no floppy writes) |
| SOL | **Not yet** — the session is refused and logged |

The bridge always sends RAW tiles. ui-toolkit's ZRLE decode path (its default when talking to real AMT) is
not exercised. That doesn't matter for input and cursor alignment, but it does matter if you're chasing rendering bugs.

## Build

Go 1.22 or newer. No third-party dependencies.

```
go build -o fake-amt .          # Linux/macOS
go build -o fake-amt.exe .      # Windows
```

Prebuilt `linux-amd64` and `windows-amd64` binaries are in `bin/`.

## Quick start

### 1. Run a VM with a VNC display

The settings that matter for cursor work:

- **`usb-tablet`**: gives an absolute pointer, which is how AMT injects the mouse. Without it, the guest gets a relative PS/2 mouse. The cursor then drifts and accelerates *regardless* of Console, which would give you false results.
- **`-vga std`**: the guest draws its own cursor into the framebuffer, so you see the cursor the way AMT's capture does. With virtio-gpu or QXL, the guest uses a hardware cursor, which may not appear in the picture at all.
- **`-k en-gb`** (or your keyboard layout): QEMU needs it to translate VNC keysyms correctly.

Linux host:
```
qemu-system-x86_64 -enable-kvm -m 4G -smp 2 \
  -drive file=win11.qcow2 -vga std \
  -device qemu-xhci -device usb-tablet -k en-gb \
  -vnc 127.0.0.1:0                       # = port 5900
```

Windows host: QEMU for Windows works the same. Use `-accel whpx` (needs the "Windows Hypervisor Platform"
feature) or leave acceleration off for a slow but working VM.

libvirt: VMs usually already have `<graphics type='vnc'>` and `<input type='tablet' bus='usb'/>`. Find the port with
`virsh vncdisplay <vm>` (`:0` = 5900).

Any other VNC server (TightVNC, UltraVNC on a real PC) should work. Use `-vnc-password` if it needs one. Only QEMU-style servers have been tested.

### 2. Run fake-amt

```
fake-amt -vnc 127.0.0.1:5900 -pass P@ssw0rd
```

It listens on the AMT ports **16992** (WS-Management) and **16994** (redirection). Console has these port numbers hard-coded, so:

- don't run fake-amt on a machine with Intel LMS (that's fine on AMD);
- to run several fake devices on one Linux box, give each its own loopback address with `-bind 127.0.0.2`, `-bind 127.0.0.3`, and so on.

### 3. Add it to Console

In Console, add a device:

- **Hostname**: the machine running fake-amt.
- **Username**: `admin`. Console's redirection code only handles a 5-character username, so keep it `admin`.
- **Password**: whatever you passed with `-pass`.
- **TLS**: off.
  - For TLS instead, run fake-amt with `-tls`, then set TLS on and allow self-signed.

Open the device and start KVM.

## Flags

| Flag | Default | |
|---|---|---|
| `-vnc` | `127.0.0.1:5900` | VNC server to show |
| `-vnc-password` | | if the VNC server needs one |
| `-user` / `-pass` | `admin` / `P@ssw0rd` | AMT credentials Console must use |
| `-bind` | `0.0.0.0` | listen address |
| `-tls`, `-tls-only` | off | also/only serve 16993/16995 with a self-signed cert |
| `-trace-pointer` | `clicks` | `none`, `clicks` (button changes + out-of-bounds), `all` (every move) |
| `-trace-keys` | off | log keysyms |
| `-trace-wsman` | off | log each WS-Management call and which recorded response answered it |
| `-fixtures DIR` | | extra/override WS-Management responses (see below) |
| `-keepalive` | `10s` | see "Idle sessions" (also paces IDER heartbeats) |
| `-hyperv-vm` | | Hyper-V VM whose DVD drive receives IDER-mounted images; see "Virtual media (IDER)" |
| `-ider-dir` | `%TEMP%ake-amt-ider` | where those images are kept while inserted |

### Pointer tracing (for the cursor investigation)

`-trace-pointer all` logs exactly what the browser sent, against the current framebuffer size:

```
redir#3: pointer x= 700 y= 500 buttons=001  fb=1024x768
redir#3: pointer x=1101 y= 220 buttons=000  fb=1024x768  <-- OUTSIDE framebuffer
```

To check alignment, click a known on-screen target in the VM, such as a corner of a window, and compare the
coordinates the browser sent with where the target really is. If the browser's numbers are wrong, the bug is in
the client. If they're right but the guest cursor ends up elsewhere, look at the guest side (pointer device, display scaling).

### Overriding device responses

Drop SOAP response XML files into a directory and pass `-fixtures DIR`. Each file is matched by the
`Action` and `ResourceURI` in its own header, and it replaces the built-in response for that class and method. Typical uses:

- report a different power state;
- turn on user consent, so you can test the consent-code flow (`IPS_OptInService` → `OptInRequired`);
- answer a call that logs `NO FIXTURE`.

Copy one of the files under `fixtures/responses/` as a starting point.

### Idle sessions

Console closes a relay when the device has sent nothing for 30 seconds. A VM showing a static desktop sends nothing.
fake-amt therefore sends a harmless RFB bell (ui-toolkit ignores it) after 10 seconds of quiet.

Use `-keepalive 0` to switch this off if you want to see Console's own timeout behaviour.

### Virtual media (IDER)

Mount a disk image from the browser (ui-toolkit's IDER component) and fake-amt answers the way AMT plus a host would:

1. opens the IDER session and acknowledges the register toggle. It logs the requested start mode (on next reboot / graceful / now), but always exposes the drive immediately;
2. sends TEST UNIT READY, READ CAPACITY and READ(10) of sector 16, logging the size and the ISO 9660 volume label it read back;
3. **with `-hyperv-vm <name>`**: reads the whole image over IDER, saves it as an `.iso` under `-ider-dir` (logging its sha256), and runs `Set-VMDvdDrive` to put it in the VM's first DVD drive;
4. idles, sending IDER heartbeats so Console's 30s timeout doesn't drop the session, until the browser unmounts. Unmounting ejects the DVD and deletes the `.iso`.

```
fake-amt -vnc 172.25.139.201:5900 -vnc-password secret -hyperv-vm "Win11 Test"
```

For the Hyper-V step:

- fake-amt must run elevated, or as a member of *Hyper-V Administrators*. It checks this at startup and warns if not.
- The VM needs a DVD drive. On a Generation 2 VM that's a SCSI DVD drive, added in Hyper-V Manager.
- The whole image is copied before it's inserted: about 10 MiB/s in the local e2e test, and probably slower through a real browser and Console. Allow several minutes for a multi-GB ISO.
- Real AMT serves sectors on demand. This copy-then-insert step is the closest Hyper-V allows.

## Tests

`test/run-e2e.sh` runs the real ui-toolkit KVM client in Node through the following chain:

1. a copy of Console's relay and interceptor code,
2. fake-amt,
3. a test VNC server that serves a known pixel pattern.

It checks:

- auth;
- the 64×64 re-tiling and colour conversion, pixel by pixel, at 16-bit and 8-bit;
- live desktop resize;
- keyboard and pointer pass-through;
- with `IDLE=1`, that the keepalive keeps a static desktop alive, plus a control run proving that Console drops it without the keepalive.

```
git clone https://github.com/device-management-toolkit/ui-toolkit && (cd ui-toolkit && npm ci && npm i --no-save tsx)
UI_TOOLKIT=$PWD/ui-toolkit ./test/run-e2e.sh
UI_TOOLKIT=$PWD/ui-toolkit IDLE=1 ./test/run-e2e.sh     # adds ~70s idle checks
```

`test/ider-client.ts` does the same for IDER: the real ui-toolkit `AMTRedirector` + `AMTIDER` mount a generated ISO through the relay. It checks:

- the session and register toggle;
- the volume label fake-amt decoded;
- with `PULL=1` (fake-amt started with `-hyperv-vm`), that the whole image was copied and its sha256 matches;
- that unmounting ends the session.

```
UI_TOOLKIT=$PWD/ui-toolkit RELAY=ws://127.0.0.1:8181/relay FAKEAMT_LOG=amt.log npx tsx test/ider-client.ts
UI_TOOLKIT=$PWD/ui-toolkit RELAY=ws://127.0.0.1:8181/relay FAKEAMT_LOG=amt.log PULL=1 npx tsx test/ider-client.ts
```

Last run: Console `2830ebd`, ui-toolkit `bad4842`, all checks passing.

## Cursor misalignment: what the test found in ui-toolkit

The e2e test also measures ui-toolkit's mouse mapping (`src/core/Utilities/MouseHelper.ts`, `mousemove`). The bridge
passes pointer events through untouched. These results describe the client, for a 1024×768 desktop, aiming at (700, 500):

| Canvas on the page | Remote got | Error |
|---|---|---|
| 1:1, or offset in the page | (700, 500) | none |
| scaled uniformly (aspect ratio kept) | (700, 500) | none |
| **fullscreen 1920×1080, letterboxed** | (870, 375) | **+170, −125** |
| **stretched non-uniformly (1000×768 element)** | (683, 512) | −17, +12 |
| **after the page scrolled 200px** | (700, 700) | **0, +200** |

The causes, all in `MouseHelper.mousemove`:

1. **Fullscreen letterboxing.** `toggleFullscreen()` calls `requestFullscreen()` on the canvas. The browser's
   built-in fullscreen style then makes the element fill the screen with `object-fit: contain`.
   - This was verified in Chrome 141: a 1280×1024 canvas fullscreened on a 1600×900 screen becomes a 1600×900 element with `object-fit: contain`.
   - The picture is centred with bars on each side, but the mapping assumes it fills the element.
   - So the error appears whenever the remote aspect ratio differs from the screen's, and it's worst at the edges.
2. **Swapped scale factors.** X is scaled by `height / offsetHeight` and Y by `width / offsetWidth`. This is only correct
   when the displayed aspect ratio exactly matches the framebuffer.
3. **Cached offsets.** `topposition` and `leftposition` keep the *largest* `getBoundingClientRect()` top and left ever seen,
   and add the difference to every later event. They are reset only on fullscreen toggle, so scrolling the page shifts every click by the scroll distance.

A mapping that handles all three cases uses the rendered content box. A sketch:

```ts
const r = canvas.getBoundingClientRect()
const s = Math.min(r.width / canvas.width, r.height / canvas.height)        // object-fit: contain
const ox = r.left + (r.width - canvas.width * s) / 2
const oy = r.top + (r.height - canvas.height * s) / 2
x = Math.round((e.clientX - ox) / s);  y = Math.round((e.clientY - oy) / s) // clamp to the framebuffer
```

## Licence notes

`fixtures/responses` comes from
[go-wsman-messages](https://github.com/device-management-toolkit/go-wsman-messages) (Apache-2.0, see `fixtures/LICENSE`),
at commit `6b4e1b8`.

`test/relay/console_interceptor.go` and `test/relay/wsman_hash.go` are verbatim copies of Console and go-wsman-messages
code (Apache-2.0), used only in the test harness.
