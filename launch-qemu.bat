@echo off

REM File Paths
set DISK_IMG=win11.qcow2
set WIN_ISO=..\Windows11_Client_x64_en-us_26300_9457.iso
set VIRTIO_ISO=..\virtio-win-0.1.302.iso
set OVMF_FW="C:\Program Files\qemu\share\edk2-x86_64-secure-code.fd"

REM QEMU Command
qemu-system-x86_64 ^
-enable-kvm ^
-accel whpx ^
-cpu host ^
-smp sockets=1,cores=4,threads=2 ^
-m 8G ^
-bios %OVMF_FW% ^
-boot menu=on,strict=on ^
-device qemu-xhci,id=usb ^
-device usb-tablet ^
-device usb-kbd ^
-k en-gb ^
-vga std ^
-vnc 127.0.0.1:0 ^
-display sdl,gl=on ^
-drive file=%DISK_IMG%,if=virtio,id=hd0,cache=writeback,format=qcow2 ^
-drive file=%WIN_ISO%,media=cdrom,id=cd0,format=raw ^
-drive file=%VIRTIO_ISO%,media=cdrom,id=cd1,format=raw ^
-netdev user,id=net0 ^
-device virtio-net-pci,netdev=net0

pause