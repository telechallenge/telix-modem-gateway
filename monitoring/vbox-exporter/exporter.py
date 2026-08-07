"""Prometheus exporter for VirtualBox VMs, measured from the host.

Everything here is observed from outside the guest: VBoxManage's view of each
VM and the host kernel's view of its VBoxHeadless process. Nothing needs Guest
Additions, nothing runs inside the VM, and the Guest/* metric family VirtualBox
offers is deliberately not exported -- it is only populated when Additions are
installed, reads empty on a DOS guest, and would be guest-reported rather than
host-observed.

This runs on the host rather than in a container because VBoxManage talks to
VBoxSVC over the session owner's XPCOM IPC socket; a container has neither the
socket nor the matching library tree. See telix-vbox-exporter.service.

Stdlib-only, matching monitoring/fail2ban-exporter and monitoring/bbs-exporter.

Where each number comes from, and why:

  * Inventory and state come from `VBoxManage list vms` plus `showvminfo
    --machinereadable`. This is the only source that can see a VM that is
    *registered but not running* -- a process scan cannot tell "powered off"
    apart from "exporter is broken", and an absent series is a bad way to
    learn a board's VM died.

  * Point-in-time gauges (CPU load, RSS, disk, net rate) come from VBoxManage's
    metrics collector, always via the `:avg` aggregate. The raw metric returns
    several comma-separated samples whose ordering is not documented; the
    aggregate is one unambiguous number.

  * CPU *counters* come from /proc/<pid>/stat instead, because the collector's
    CPU/Load is a pre-averaged percentage over its own sampling window, and
    Prometheus would rather rate() a monotonic counter itself.

  * /proc/<pid>/io is not read. Ubuntu's yama ptrace_scope=1 denies it even to
    the same uid that owns the process (measured), so per-VM disk IO would be
    permanently empty rather than merely unavailable.

Units: VirtualBox reports memory in kB and disk in MB, both binary multiples.
Confirmed against this host rather than assumed -- RAM/Usage/Used for a VM
matched ps RSS to within a rounding step, and Disk/Usage/Used matched the sum
of the VM's VDI files in bytes / 1024^2 exactly.
"""
import argparse
import os
import re
import subprocess
import sys
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

DEFAULT_PORT = 9103

# docker0's host address. Every container on every Docker network can reach it
# (measured from a container on the compose-created telix-net), and it is what
# Docker's `host-gateway` alias resolves to here, so Prometheus scrapes this
# exporter as host.docker.internal:9103. It is a host-local bridge address, so
# unlike 0.0.0.0 it does not put the exporter on the LAN.
DEFAULT_BIND = "172.17.0.1"

# Sampling window for VBoxManage's metrics collector.
METRICS_PERIOD = 5
METRICS_SAMPLES = 3

# The metrics pulled from the collector, as (VBox metric, exported name, help,
# converter). Only :avg aggregates -- see the module docstring.
def PERCENT(v: float) -> float:
    return v / 100.0


def KIB(v: float) -> float:
    return v * 1024


def MIB(v: float) -> float:
    return v * 1024 * 1024


def IDENT(v: float) -> float:
    return v

VM_METRICS = (
    ("CPU/Load/User:avg", "vbox_vm_cpu_load_ratio", "user", PERCENT),
    ("CPU/Load/Kernel:avg", "vbox_vm_cpu_load_ratio", "kernel", PERCENT),
    ("RAM/Usage/Used:avg", "vbox_vm_memory_resident_bytes", None, KIB),
    ("Disk/Usage/Used:avg", "vbox_vm_disk_bytes", None, MIB),
    ("Net/Rate/Rx:avg", "vbox_vm_network_receive_bytes_per_second", None, IDENT),
    ("Net/Rate/Tx:avg", "vbox_vm_network_transmit_bytes_per_second", None, IDENT),
)

HOST_METRICS = (
    ("RAM/Usage/Used:avg", "vbox_host_memory_used_bytes",
     "Physical memory in use on the host, as VirtualBox sees it.", KIB),
    ("RAM/VMM/Used:avg", "vbox_host_vmm_memory_bytes",
     "Physical memory used by the hypervisor itself.", KIB),
)

# `VBoxManage list vms` -> "name" {uuid}. The name is quoted, so it may contain
# spaces and even braces; anchor on the trailing brace-wrapped UUID instead of
# splitting on whitespace.
LIST_VMS_RE = re.compile(r'^"(?P<name>.*)"\s+\{(?P<uuid>[0-9a-fA-F-]{36})\}\s*$')

# showvminfo --machinereadable emits key="value" and key=value alike.
KV_RE = re.compile(r'^(?P<key>[^=]+)=(?P<value>.*)$')

# VBoxHeadless is started as `... --startvm <uuid> ...`; --comment carries the
# name but the UUID is the stable key.
STARTVM_RE = re.compile(r"--startvm[\s\0]+([0-9a-fA-F-]{36})")

CLK_TCK = os.sysconf("SC_CLK_TCK")


def escape(value: object) -> str:
    """Escape a Prometheus label value (backslash, quote, newline)."""
    return str(value).replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n")


class VBoxError(RuntimeError):
    """VBoxManage could not be run, or failed."""


def vboxmanage(binary: str, *args: str, timeout: int = 15) -> str:
    """Run VBoxManage and return stdout, raising VBoxError on any failure."""
    try:
        proc = subprocess.run(
            [binary, *args],
            capture_output=True,
            text=True,
            timeout=timeout,
            check=False,
        )
    except (OSError, subprocess.SubprocessError) as err:
        raise VBoxError("%s %s: %s" % (binary, " ".join(args), err)) from err
    if proc.returncode != 0:
        raise VBoxError(
            "%s %s exited %d: %s"
            % (binary, " ".join(args), proc.returncode, proc.stderr.strip()[:200])
        )
    return proc.stdout


def list_vms(binary: str) -> list[tuple[str, str]]:
    """[(name, uuid)] for every registered VM, running or not."""
    vms = []
    for line in vboxmanage(binary, "list", "vms").splitlines():
        match = LIST_VMS_RE.match(line.strip())
        if match:
            vms.append((match.group("name"), match.group("uuid")))
    return vms


def vm_info(binary: str, uuid: str) -> dict[str, str]:
    """showvminfo --machinereadable as a dict, values unquoted."""
    info = {}
    for line in vboxmanage(binary, "showvminfo", uuid, "--machinereadable").splitlines():
        match = KV_RE.match(line.strip())
        if match:
            info[match.group("key")] = match.group("value").strip('"')
    return info


def parse_metrics(text: str) -> dict[tuple[str, str], float]:
    """{(object, metric): value} from `VBoxManage metrics query` output.

    Column boundaries are taken from the dashed separator line rather than by
    splitting on whitespace: VirtualBox sizes the columns to their content and
    a VM name may contain spaces, which would silently shift every field.
    """
    lines = text.splitlines()
    sep = next(
        (i for i, ln in enumerate(lines) if ln.startswith("---") and " " in ln),
        None,
    )
    if sep is None:
        return {}

    widths = [len(chunk) for chunk in lines[sep].split(" ") if chunk]
    if len(widths) < 3:
        return {}
    obj_end = widths[0]
    metric_end = obj_end + 1 + widths[1]

    out = {}
    for line in lines[sep + 1:]:
        if not line.strip():
            continue
        obj = line[:obj_end].strip()
        metric = line[obj_end:metric_end].strip()
        values = line[metric_end:].strip()
        if not obj or not metric or not values:
            continue  # a metric with no samples yet
        # "0.05%" / "72220 kB" / "399 MB" / "0 B/s"; take the first sample and
        # drop its unit suffix. :avg rows carry exactly one.
        first = values.split(",")[0].strip()
        number = first.split(" ")[0].rstrip("%")
        try:
            out[(obj, metric)] = float(number)
        except ValueError:
            continue
    return out


def metrics_query(binary: str) -> dict[tuple[str, str], float]:
    return parse_metrics(vboxmanage(binary, "metrics", "query"))


def setup_metrics(binary: str) -> None:
    """(Re)arm the collector. Safe to repeat; ignored if it fails.

    The collector's configuration lives in VBoxSVC, which runs with
    --auto-shutdown and exits once its last client goes away. A running VM is
    itself a client, so in practice the setup survives as long as any VM is up
    -- but it is lost across a window where every VM was powered off, and the
    metrics then come back empty forever. Re-arming when a running VM has no
    samples is what makes that self-healing.
    """
    try:
        vboxmanage(
            binary, "metrics", "setup",
            "--period", str(METRICS_PERIOD),
            "--samples", str(METRICS_SAMPLES),
        )
    except VBoxError as err:
        print("metrics setup failed: %s" % err, file=sys.stderr, flush=True)


def headless_pids() -> dict[str, int]:
    """{vm_uuid: pid} for every running VBoxHeadless process."""
    found = {}
    for entry in os.listdir("/proc"):
        if not entry.isdigit():
            continue
        try:
            with open("/proc/%s/cmdline" % entry, "rb") as fh:
                cmdline = fh.read().decode("utf-8", "replace")
        except OSError:
            continue  # the process exited between listdir and open
        if "VBoxHeadless" not in cmdline and "VirtualBoxVM" not in cmdline:
            continue
        match = STARTVM_RE.search(cmdline)
        if match:
            found[match.group(1).lower()] = int(entry)
    return found


def boot_time() -> float | None:
    """Unix time the host booted, for turning proc starttime into an epoch."""
    try:
        with open("/proc/stat") as fh:
            for line in fh:
                if line.startswith("btime "):
                    return float(line.split()[1])
    except (OSError, ValueError, IndexError):
        pass
    return None


def proc_stats(pid: int) -> dict[str, float]:
    """CPU seconds, thread count and start time for one process.

    /proc/<pid>/io is deliberately not read -- see the module docstring.
    """
    stats: dict[str, float] = {}
    try:
        with open("/proc/%d/stat" % pid) as fh:
            raw = fh.read()
    except OSError:
        return stats
    # comm sits in parentheses and may itself contain spaces and parens, so the
    # fields are counted from the last ')' rather than from the start.
    close = raw.rfind(")")
    if close == -1:
        return stats
    fields = raw[close + 2:].split()
    # fields[0] is field 3 (state), so field N is fields[N - 3].
    try:
        stats["cpu_user"] = float(fields[11]) / CLK_TCK  # field 14, utime
        stats["cpu_system"] = float(fields[12]) / CLK_TCK  # field 15, stime
        stats["threads"] = float(fields[17])  # field 20, num_threads
        boot = boot_time()
        if boot is not None:
            stats["start_time"] = boot + float(fields[19]) / CLK_TCK  # field 22
    except (IndexError, ValueError):
        return stats

    try:
        with open("/proc/%d/status" % pid) as fh:
            for line in fh:
                if line.startswith("VmRSS:"):
                    stats["rss"] = float(line.split()[1]) * 1024
                    break
    except (OSError, ValueError, IndexError):
        pass
    return stats


class Exposition:
    """Accumulates samples so each metric family emits its HELP/TYPE once."""

    def __init__(self) -> None:
        self._families: list[tuple[str, str, str, list[tuple[dict[str, str], object]]]] = []
        self._index: dict[str, int] = {}

    def add(self, name: str, kind: str, help_text: str, value: object,
            labels: dict[str, str] | None = None) -> None:
        if name not in self._index:
            self._index[name] = len(self._families)
            self._families.append((name, kind, help_text, []))
        self._families[self._index[name]][3].append((labels or {}, value))

    def render(self) -> str:
        out = []
        for name, kind, help_text, samples in self._families:
            out.append("# HELP %s %s" % (name, help_text))
            out.append("# TYPE %s %s" % (name, kind))
            for labels, value in samples:
                rendered = ",".join(
                    '%s="%s"' % (k, escape(v)) for k, v in sorted(labels.items())
                )
                out.append(
                    "%s{%s} %s" % (name, rendered, value)
                    if rendered
                    else "%s %s" % (name, value)
                )
        return "\n".join(out) + "\n"


def _emit_vm(exp: Exposition, name: str, uuid: str, info: dict[str, str],
             samples: dict[tuple[str, str], float], pid: int | None) -> None:
    """Every metric family for one registered VM."""
    lbl = {"vm": name}
    state = info.get("VMState", "unknown")
    running = state == "running"

    exp.add(
        "vbox_vm_info", "gauge",
        "VM identity. UUID and guest OS type are labels here rather than on "
        "every series so a rename does not orphan the history.",
        1, {"vm": name, "uuid": uuid, "ostype": info.get("ostype", "unknown")},
    )
    # Only the current state is emitted. Emitting every state as 0/1 would be
    # six series per VM forever; this is one, and `vbox_vm_state{state="X"}`
    # simply goes absent when the VM leaves state X.
    exp.add(
        "vbox_vm_state", "gauge",
        "The VM's current state as a label (running, poweroff, paused, saved, "
        "aborted, ...). Only the state the VM is actually in is emitted.",
        1, {"vm": name, "state": state},
    )
    exp.add(
        "vbox_vm_running", "gauge",
        "1 when the VM is running. Registered-but-stopped VMs still report 0 "
        "here, so a VM that dies is a falling line rather than a vanished one.",
        1 if running else 0, lbl,
    )

    for key, metric, help_text, scale in (
        ("memory", "vbox_vm_configured_memory_bytes",
         "RAM the VM is configured with, whether or not it is running.", 1024 * 1024),
        ("cpus", "vbox_vm_configured_vcpus",
         "Virtual CPUs the VM is configured with.", 1),
    ):
        try:
            exp.add(metric, "gauge", help_text, int(info[key]) * scale, lbl)
        except (KeyError, ValueError):
            pass

    for vbox_metric, metric, mode, convert in VM_METRICS:
        value = samples.get((name, vbox_metric))
        if value is None:
            continue
        labels = dict(lbl, mode=mode) if mode else lbl
        exp.add(
            metric, "gauge",
            {
                "vbox_vm_cpu_load_ratio":
                    "Share of one host CPU the VM process is using, 0-1, as "
                    "measured by VirtualBox on the host.",
                "vbox_vm_memory_resident_bytes":
                    "Resident size of the VM process on the host.",
                "vbox_vm_disk_bytes":
                    "Actual size of all the VM's virtual disks combined.",
                "vbox_vm_network_receive_bytes_per_second":
                    "VM network receive rate. Reads zero for adapter types "
                    "VirtualBox does not meter.",
                "vbox_vm_network_transmit_bytes_per_second":
                    "VM network transmit rate. Reads zero for adapter types "
                    "VirtualBox does not meter.",
            }[metric],
            "%.6f" % convert(value), labels,
        )

    if pid is None:
        return
    stats = proc_stats(pid)
    for key, mode in (("cpu_user", "user"), ("cpu_system", "system")):
        if key in stats:
            exp.add(
                "vbox_vm_cpu_seconds_total", "counter",
                "CPU seconds consumed by the VM process on the host, read from "
                "/proc. A counter, so Prometheus can rate() it itself rather "
                "than taking VirtualBox's pre-averaged percentage.",
                "%.2f" % stats[key], dict(lbl, mode=mode),
            )
    if "rss" in stats:
        exp.add(
            "vbox_vm_process_resident_bytes", "gauge",
            "Resident set size of the VM process, read from /proc. Corroborates "
            "vbox_vm_memory_resident_bytes from an independent source.",
            int(stats["rss"]), lbl,
        )
    if "threads" in stats:
        exp.add("vbox_vm_threads", "gauge",
                "Threads in the VM process.", int(stats["threads"]), lbl)
    if "start_time" in stats:
        exp.add("vbox_vm_start_time_seconds", "gauge",
                "Unix time the VM process started; subtract from now() for uptime.",
                "%.2f" % stats["start_time"], lbl)


def collect(binary: str = "VBoxManage", pids: dict[str, int] | None = None) -> str:
    """Render the exposition text for one scrape.

    pids is the {vm_uuid: pid} map, injectable so tests need not depend on
    whatever VMs happen to be running on the machine running them.
    """
    started = time.time()
    exp = Exposition()

    try:
        vms = list_vms(binary)
    except VBoxError as err:
        print("scrape failed: %s" % err, file=sys.stderr, flush=True)
        exp.add("vbox_up", "gauge",
                "Whether VBoxManage could be run and VBoxSVC answered.", 0)
        return exp.render()

    try:
        samples = metrics_query(binary)
    except VBoxError as err:
        print("metrics query failed: %s" % err, file=sys.stderr, flush=True)
        samples = {}

    if pids is None:
        pids = headless_pids()

    # Self-heal the collector: if something is running but VirtualBox has no
    # CPU sample for it, VBoxSVC was restarted and lost its setup.
    if pids and not any(metric.startswith("CPU/Load") for _obj, metric in samples):
        setup_metrics(binary)
        try:
            samples = metrics_query(binary)
        except VBoxError:
            pass

    running = 0
    for name, uuid in vms:
        try:
            info = vm_info(binary, uuid)
        except VBoxError as err:
            print("showvminfo %s failed: %s" % (uuid, err), file=sys.stderr, flush=True)
            info = {}
        if info.get("VMState") == "running":
            running += 1
        _emit_vm(exp, name, uuid, info, samples, pids.get(uuid.lower()))

    for vbox_metric, metric, help_text, convert in HOST_METRICS:
        value = samples.get(("host", vbox_metric))
        if value is not None:
            exp.add(metric, "gauge", help_text, "%.0f" % convert(value))

    exp.add("vbox_vms_registered", "gauge",
            "VMs registered with VirtualBox, running or not.", len(vms))
    exp.add("vbox_vms_running", "gauge", "VMs currently running.", running)
    exp.add("vbox_scrape_duration_seconds", "gauge",
            "Time taken to shell out to VBoxManage and read /proc for this scrape.",
            "%.6f" % (time.time() - started))
    exp.add("vbox_up", "gauge",
            "Whether VBoxManage could be run and VBoxSVC answered.", 1)
    return exp.render()


class Handler(BaseHTTPRequestHandler):
    binary = "VBoxManage"

    def do_GET(self):
        if self.path.split("?")[0] not in ("/metrics", "/"):
            self.send_error(404)
            return
        body = collect(self.binary).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format, *args):  # noqa: A002 - signature is the base class's
        pass  # a scrape every 15s is not news


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--bind", default=os.environ.get("BIND", DEFAULT_BIND))
    ap.add_argument("--port", type=int, default=int(os.environ.get("PORT", DEFAULT_PORT)))
    ap.add_argument("--vboxmanage", default=os.environ.get("VBOXMANAGE", "VBoxManage"))
    args = ap.parse_args()

    Handler.binary = args.vboxmanage
    setup_metrics(args.vboxmanage)
    try:
        server = HTTPServer((args.bind, args.port), Handler)
    except OSError as err:
        # Almost always "Cannot assign requested address" because docker0 is
        # absent or on a different subnet. Say so, rather than dying on a
        # bare errno.
        print(
            "cannot bind %s:%d (%s).\nThis address should be the host's Docker "
            "bridge address -- check `ip -4 addr show docker0` and pass --bind "
            "if it differs." % (args.bind, args.port, err),
            file=sys.stderr, flush=True,
        )
        raise SystemExit(1) from err
    print("virtualbox exporter on %s:%d" % (args.bind, args.port), flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
