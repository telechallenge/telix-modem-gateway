"""Tests for the VirtualBox Prometheus exporter.

Run: python3 -m unittest discover -s monitoring/vbox-exporter

The VBoxManage fixtures are real output captured from the host this was built
against, not a convenient paraphrase -- the column alignment and the unit
suffixes are exactly the things a hand-written sample would get wrong.

collect() is exercised through a stub VBoxManage on disk rather than by patching
subprocess, so the argument handling and the output parsing are both under test.
"""
import os
import stat
import tempfile
import textwrap
import unittest

from exporter import (
    collect,
    list_vms,
    parse_metrics,
    proc_stats,
    vm_info,
)

WINXP_UUID = "5a22fef0-6312-4e46-a96e-86a2aa4ddf5a"
MAJORBBS_UUID = "f102cf88-4794-44d1-bcba-1eaf0ab1e2d3"

# Real `VBoxManage metrics query` output. Note the ":avg" rows carry one value
# and the bare rows carry three, and that units differ per metric.
METRICS_QUERY = """\
Object          Metric                                   Values
--------------- ---------------------------------------- ------------------------
host            RAM/Usage/Used:avg                       7622096 kB
host            RAM/VMM/Used:avg                         1889284 kB
majorbbs        CPU/Load/User:avg                        0.05%
majorbbs        CPU/Load/Kernel:avg                      12.55%
majorbbs        RAM/Usage/Used:avg                       72220 kB
majorbbs        Disk/Usage/Used:avg                      399 MB
majorbbs        Net/Rate/Rx:avg                          0 B/s
majorbbs        Net/Rate/Tx:avg                          0 B/s
majorbbs        Guest/CPU/Load/User:avg
WinXP           CPU/Load/User:avg                        0.10%
WinXP           CPU/Load/Kernel:avg                      1.08%
WinXP           RAM/Usage/Used:avg                       1816052 kB
WinXP           Disk/Usage/Used:avg                      7037 MB
WinXP           Net/Rate/Rx:avg                          0 B/s
WinXP           Net/Rate/Tx:avg                          0 B/s
"""

LIST_VMS = (
    '"WinXP" {5a22fef0-6312-4e46-a96e-86a2aa4ddf5a}\n'
    '"majorbbs" {f102cf88-4794-44d1-bcba-1eaf0ab1e2d3}\n'
)

SHOWVMINFO = {
    WINXP_UUID: (
        'name="WinXP"\nostype="Windows XP (32-bit)"\nmemory=4096\ncpus=1\n'
        'VMState="running"\nVMStateChangeTime="2026-08-05T03:22:31.094000000"\n'
    ),
    MAJORBBS_UUID: (
        'name="majorbbs"\nostype="DOS"\nmemory=64\ncpus=1\n'
        'VMState="poweroff"\nVMStateChangeTime="2026-08-05T03:22:31.094000000"\n'
    ),
}

STUB = textwrap.dedent(
    """\
    #!%(python)s
    import sys
    args = sys.argv[1:]
    if %(fail)r:
        sys.stderr.write("VBoxManage: error: could not connect\\n")
        sys.exit(1)
    if args[:2] == ["list", "vms"]:
        sys.stdout.write(%(list_vms)r)
    elif args[0] == "showvminfo":
        sys.stdout.write(%(info)r.get(args[1], ""))
    elif args[:2] == ["metrics", "query"]:
        sys.stdout.write(%(metrics)r)
    elif args[:2] == ["metrics", "setup"]:
        pass
    else:
        sys.exit(2)
    """
)


def sample(text, metric, **labels):
    """The value of one sample, matched by metric name and exact labels."""
    want = ",".join('%s="%s"' % (k, v) for k, v in sorted(labels.items()))
    prefix = "%s{%s} " % (metric, want) if want else "%s " % metric
    for line in text.splitlines():
        if line.startswith(prefix):
            return line[len(prefix):]
    return None


def type_of(text, metric):
    for line in text.splitlines():
        if line.startswith("# TYPE %s " % metric):
            return line.split()[-1]
    return None


class VBoxExporterTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)

    def collect(self, **kw):
        """collect() against a stub VBoxManage, with the running VM's process
        pinned to this interpreter so the suite is hermetic: scanning the real
        /proc would make these assertions depend on whatever VMs the machine
        running the tests happens to have up."""
        pids = kw.pop("pids", {WINXP_UUID: os.getpid()})
        return collect(self.stub(**kw), pids=pids)

    def stub(self, fail=False, list_vms_out=LIST_VMS, metrics=METRICS_QUERY):
        path = os.path.join(self.tmp.name, "VBoxManage")
        with open(path, "w") as fh:
            fh.write(STUB % {
                "python": "/usr/bin/env python3",
                "fail": fail,
                "list_vms": list_vms_out,
                "info": SHOWVMINFO,
                "metrics": metrics,
            })
        os.chmod(path, os.stat(path).st_mode | stat.S_IEXEC)
        return path

    def test_units_are_converted_the_way_the_host_actually_reports_them(self):
        """VirtualBox's kB and MB are binary multiples and CPU load is a
        percentage. Confirmed against the host: RAM/Usage/Used matched ps RSS,
        and Disk/Usage/Used matched the sum of the VDI files / 1024^2."""
        out = self.collect()

        self.assertEqual(sample(out, "vbox_vm_memory_resident_bytes", vm="WinXP"),
                         "%.6f" % (1816052 * 1024))
        self.assertEqual(sample(out, "vbox_vm_disk_bytes", vm="WinXP"),
                         "%.6f" % (7037 * 1024 * 1024))
        self.assertEqual(sample(out, "vbox_vm_cpu_load_ratio", vm="WinXP", mode="kernel"),
                         "%.6f" % 0.0108)
        self.assertEqual(sample(out, "vbox_host_memory_used_bytes"),
                         "%.0f" % (7622096 * 1024))

    def test_a_stopped_vm_still_reports_itself_as_stopped(self):
        """A process scan cannot tell "powered off" from "exporter broken".
        The registered-VM inventory is what makes a dead VM a falling line
        rather than a vanished series."""
        out = self.collect()

        self.assertEqual(sample(out, "vbox_vm_running", vm="majorbbs"), "0")
        self.assertEqual(sample(out, "vbox_vm_state", vm="majorbbs", state="poweroff"), "1")
        self.assertIsNone(sample(out, "vbox_vm_state", vm="majorbbs", state="running"))
        self.assertEqual(sample(out, "vbox_vm_running", vm="WinXP"), "1")
        self.assertEqual(sample(out, "vbox_vms_registered"), "2")
        self.assertEqual(sample(out, "vbox_vms_running"), "1")
        # Configured hardware is reported whether or not the VM is up.
        self.assertEqual(sample(out, "vbox_vm_configured_memory_bytes", vm="majorbbs"),
                         str(64 * 1024 * 1024))
        self.assertEqual(sample(out, "vbox_vm_configured_vcpus", vm="majorbbs"), "1")

    def test_guest_metrics_are_never_exported(self):
        """Guest/* needs Guest Additions and is guest-reported. The fixture
        carries an empty Guest/CPU/Load/User row precisely because the DOS VM
        emits one. Asserted over metric names only -- the word also appears
        legitimately in vbox_vm_info's help text ("guest OS type")."""
        names = {
            ln.split("{")[0].split(" ")[0]
            for ln in self.collect().splitlines()
            if ln and not ln.startswith("#")
        }
        self.assertTrue(names)
        self.assertEqual([n for n in names if "guest" in n.lower()], [])

    def test_vboxmanage_failure_reports_down_rather_than_a_blank_page(self):
        out = self.collect(fail=True)
        self.assertEqual(sample(out, "vbox_up"), "0")
        self.assertIsNone(sample(out, "vbox_vms_registered"))

    def test_cpu_seconds_is_a_counter_and_load_is_a_gauge(self):
        """Prometheus should rate() a monotonic counter itself rather than
        take VirtualBox's pre-averaged percentage as if it were one."""
        out = self.collect()
        self.assertEqual(type_of(out, "vbox_vm_cpu_load_ratio"), "gauge")
        self.assertEqual(type_of(out, "vbox_vm_cpu_seconds_total"), "counter")

    def test_process_series_are_absent_for_a_vm_with_no_process(self):
        """majorbbs is powered off in the fixture, so nothing should claim to
        have read its CPU time off the host."""
        out = self.collect()
        self.assertIsNone(sample(out, "vbox_vm_threads", vm="majorbbs"))
        self.assertIsNone(
            sample(out, "vbox_vm_cpu_seconds_total", vm="majorbbs", mode="user")
        )
        self.assertIsNotNone(sample(out, "vbox_vm_threads", vm="WinXP"))

    def test_metrics_columns_survive_a_vm_name_containing_spaces(self):
        """VirtualBox sizes its columns to content, so splitting on whitespace
        would shift every field for a VM named "My BBS"."""
        text = (
            "Object          Metric                         Values\n"
            "--------------- ------------------------------ ----------------\n"
            "My Old BBS      CPU/Load/User:avg              3.25%\n"
            "My Old BBS      RAM/Usage/Used:avg             1024 kB\n"
        )
        parsed = parse_metrics(text)
        self.assertEqual(parsed[("My Old BBS", "CPU/Load/User:avg")], 3.25)
        self.assertEqual(parsed[("My Old BBS", "RAM/Usage/Used:avg")], 1024.0)

    def test_metric_rows_with_no_samples_are_skipped_not_zeroed(self):
        """An empty Guest/* row means "not measurable", which is not zero."""
        parsed = parse_metrics(METRICS_QUERY)
        self.assertNotIn(("majorbbs", "Guest/CPU/Load/User:avg"), parsed)
        self.assertIn(("majorbbs", "CPU/Load/User:avg"), parsed)

    def test_vm_names_with_spaces_and_braces_survive_list_vms(self):
        path = self.stub(list_vms_out='"My {Weird} BBS" {%s}\n' % WINXP_UUID)
        self.assertEqual(list_vms(path), [("My {Weird} BBS", WINXP_UUID)])

    def test_showvminfo_values_are_unquoted(self):
        info = vm_info(self.stub(), WINXP_UUID)
        self.assertEqual(info["ostype"], "Windows XP (32-bit)")
        self.assertEqual(info["VMState"], "running")
        self.assertEqual(info["memory"], "4096")

    def test_proc_field_offsets_are_right_against_a_real_process(self):
        """The offsets into /proc/<pid>/stat are counted from the last ')', so
        this asserts against the running interpreter rather than a fixture."""
        stats = proc_stats(os.getpid())
        self.assertGreaterEqual(stats["cpu_user"], 0.0)
        self.assertLess(stats["cpu_user"], 3600.0)
        self.assertGreaterEqual(stats["threads"], 1)
        self.assertLess(stats["threads"], 10000)
        self.assertGreater(stats["rss"], 1024 * 1024)
        # Started in the past, but not before the machine booted.
        import time as _time
        self.assertLess(stats["start_time"], _time.time() + 1)
        self.assertGreater(stats["start_time"], _time.time() - 86400 * 365)

    def test_a_dead_pid_yields_no_stats_rather_than_raising(self):
        self.assertEqual(proc_stats(2**21), {})


if __name__ == "__main__":
    unittest.main()
