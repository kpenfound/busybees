#!/bin/sh
# Runs core/agent's Landlock enforcement tests on a kernel that has Landlock,
# for a machine whose Dagger engine does not (the tests skip there). It boots
# Debian's kernel under QEMU with no privileges and no network, with the test
# binary, a static busybox and the container's git as the whole machine. CONTRIBUTING.md has the
# command that runs it, inside a golang:1.26-trixie container with the
# repository at /src. TESTRUN selects the tests (default: Landlock).
set -eu

export DEBIAN_FRONTEND=noninteractive
case "$(uname -m)" in
aarch64) packages="qemu-system-arm linux-image-arm64" ;;
x86_64) packages="qemu-system-x86 linux-image-amd64" ;;
*) echo "unsupported architecture $(uname -m)" >&2; exit 2 ;;
esac
apt-get update -qq >/dev/null
# shellcheck disable=SC2086
apt-get install -y -qq --no-install-recommends $packages busybox-static cpio >/dev/null

root=/tmp/machine
mkdir -p "$root/usr/bin" "$root/usr/lib" "$root/dev" "$root/proc" "$root/sys" "$root/tmp" "$root/etc"
ln -s usr/bin "$root/bin"
ln -s usr/lib "$root/lib"
cp /bin/busybox "$root/usr/bin/busybox"
# The machine's own git, where Debian keeps it, with the libraries it loads:
# the executables a turn without VCS is denied inside the system paths.
cp -a /usr/bin/git "$root/usr/bin/git"
cp -a /usr/lib/git-core "$root/usr/lib/git-core"
ldd /usr/bin/git | awk '$3 ~ /^\// {print $3} $1 ~ /^\// {print $1}' | while read -r lib; do
	mkdir -p "$root$(dirname "$(realpath "$lib")")"
	cp "$(realpath "$lib")" "$root$(realpath "$lib")"
	[ -e "$root$lib" ] || { mkdir -p "$root$(dirname "$lib")"; ln -s "$(realpath "$lib")" "$root$lib"; }
done
(cd /src/core && CGO_ENABLED=0 go test -c -o "$root/agent.test" ./agent)
echo "${TESTRUN:-Landlock}" > "$root/testrun"
cat > "$root/init" <<'INIT'
#!/bin/busybox sh
/bin/busybox --install -s /usr/bin
mount -t proc proc /proc
mount -t sysfs sys /sys
mount -t devtmpfs dev /dev
mount -t tmpfs tmp /tmp
mount -t securityfs security /sys/kernel/security
export PATH=/bin:/usr/bin HOME=/tmp TMPDIR=/tmp
echo "=== kernel $(uname -r), lsm $(cat /sys/kernel/security/lsm)"
cd /tmp
/agent.test -test.v -test.count=1 -test.run "$(cat /testrun)" 2>&1
echo "=== tests exited $?"
poweroff -f
INIT
chmod +x "$root/init"
(cd "$root" && find . | cpio -o -H newc 2>/dev/null | gzip -1 > /tmp/machine.gz)

kernel=$(find /boot -name 'vmlinuz-*' | head -1)
case "$(uname -m)" in
aarch64) set -- qemu-system-aarch64 -M virt -cpu max ; console=ttyAMA0 ;;
x86_64) set -- qemu-system-x86_64 -cpu max ; console=ttyS0 ;;
esac
timeout 900 "$@" -smp 2 -m 1536 -nographic -no-reboot -nic none \
	-kernel "$kernel" -initrd /tmp/machine.gz \
	-append "console=$console panic=-1 quiet lsm=landlock" | tee /tmp/machine.log
grep -q "=== tests exited 0" /tmp/machine.log
