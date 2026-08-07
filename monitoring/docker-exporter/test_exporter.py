"""Tests for the Docker container Prometheus exporter.

Run: python3 -m unittest discover -s monitoring/docker-exporter

The API payloads are shaped like the real ones from this host's Docker 29 --
including cgroup-v2 memory_stats and Docker's 9-digit fractional timestamps,
both of which a hand-simplified fixture gets wrong.

collect() runs against a real HTTP server on a loopback port rather than a
patched http.client, so the request paths and query strings are under test too.
"""
import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

from exporter import collect, parse_started_at

CONTAINERS = [
    {"Id": "a" * 64, "Names": ["/enigma-bbs1"], "State": "running",
     "Image": "enigmabbs/enigma-bbs@sha256:2477"},
    {"Id": "b" * 64, "Names": ["/warez-server"], "State": "exited",
     "Image": "warez-server:latest"},
]

INSPECT = {
    "a" * 64: {"RestartCount": 2,
               "HostConfig": {"Memory": 536_870_912},
               "State": {"StartedAt": "2026-08-07T00:22:21.937457335Z",
                         "Health": {"Status": "healthy"}}},
    # HostConfig.Memory 0 is Docker's "unlimited".
    "b" * 64: {"RestartCount": 0,
               "HostConfig": {"Memory": 0},
               "State": {"StartedAt": "2026-08-05T01:00:00.000000000Z"}},
}

STATS = {
    "cpu_stats": {"cpu_usage": {"total_usage": 42_788_824_000}, "online_cpus": 8},
    "memory_stats": {
        "usage": 88_969_216,
        # What Docker actually puts here for a container with no limit: the
        # host's total RAM. It must never reach a panel as a "limit".
        "limit": 16_768_585_728,
        # cgroup v2. inactive_file is the part that must come back off usage.
        "stats": {"anon": 88_000_000, "file": 969_216, "inactive_file": 446_464},
    },
    "networks": {"eth0": {"rx_bytes": 1612, "tx_bytes": 325},
                 "eth1": {"rx_bytes": 400, "tx_bytes": 75}},
    "pids_stats": {"current": 22},
}


class FakeDocker(BaseHTTPRequestHandler):
    fail = False
    seen: list = []

    def do_GET(self):
        FakeDocker.seen.append(self.path)
        if FakeDocker.fail:
            self.send_error(500)
            return
        if "/containers/json" in self.path:
            body = json.dumps(CONTAINERS)
        elif self.path.endswith("/stats") or "/stats?" in self.path:
            cid = self.path.split("/containers/")[1].split("/")[0]
            body = json.dumps(STATS if cid == "a" * 64 else {})
        elif self.path.endswith("/json"):
            cid = self.path.split("/containers/")[1].split("/")[0]
            body = json.dumps(INSPECT.get(cid, {}))
        else:
            self.send_error(404)
            return
        raw = body.encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, *args):
        pass


def sample(text, metric, **labels):
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


class DockerExporterTests(unittest.TestCase):
    def setUp(self):
        FakeDocker.fail = False
        FakeDocker.seen = []
        self.server = HTTPServer(("127.0.0.1", 0), FakeDocker)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.proxy = "127.0.0.1:%d" % self.server.server_address[1]

    def test_memory_is_a_working_set_not_raw_usage(self):
        """Raw usage counts reclaimable page cache and reads alarmingly high on
        a container that has merely touched a lot of files. The working set is
        what an OOM kill actually tracks."""
        out = collect(self.proxy)
        self.assertEqual(
            sample(out, "docker_container_memory_bytes", name="enigma-bbs1"),
            str(88_969_216 - 446_464),
        )
        self.assertEqual(
            sample(out, "docker_container_memory_limit_bytes", name="enigma-bbs1"),
            str(536_870_912),
        )

    def test_an_unlimited_container_gets_no_limit_series(self):
        """memory_stats.limit reports the host's total RAM for an unlimited
        container. Plotted as a limit that is a 16 GiB line flattening a 10 MiB
        working set onto the axis, so the limit comes from HostConfig.Memory
        and is omitted when it is 0."""
        out = collect(self.proxy)
        self.assertIsNone(
            sample(out, "docker_container_memory_limit_bytes", name="warez-server"))
        # ...and the host total that stats reported is nowhere in the output.
        self.assertNotIn(str(STATS["memory_stats"]["limit"]), out)

    def test_cpu_is_exported_as_seconds_and_typed_as_a_counter(self):
        out = collect(self.proxy)
        self.assertEqual(
            sample(out, "docker_container_cpu_seconds_total", name="enigma-bbs1"),
            "%.3f" % 42.788824,
        )
        self.assertEqual(type_of(out, "docker_container_cpu_seconds_total"), "counter")

    def test_network_is_summed_across_every_interface(self):
        out = collect(self.proxy)
        self.assertEqual(
            sample(out, "docker_container_network_receive_bytes_total", name="enigma-bbs1"),
            str(1612 + 400),
        )
        self.assertEqual(
            sample(out, "docker_container_network_transmit_bytes_total", name="enigma-bbs1"),
            str(325 + 75),
        )

    def test_a_stopped_container_reports_zero_rather_than_disappearing(self):
        out = collect(self.proxy)
        self.assertEqual(sample(out, "docker_container_running", name="warez-server"), "0")
        self.assertEqual(sample(out, "docker_container_running", name="enigma-bbs1"), "1")
        # ...and is not asked for stats it cannot have.
        self.assertIsNone(
            sample(out, "docker_container_cpu_seconds_total", name="warez-server")
        )
        self.assertNotIn("/containers/%s/stats" % ("b" * 64),
                         " ".join(FakeDocker.seen))

    def test_restarts_are_a_gauge(self):
        """RestartCount resets to zero when a container is recreated. As a
        counter that reset would become imaginary restarts under rate()."""
        out = collect(self.proxy)
        self.assertEqual(type_of(out, "docker_container_restarts"), "gauge")
        self.assertEqual(sample(out, "docker_container_restarts", name="enigma-bbs1"), "2")

    def test_health_is_a_label_and_defaults_to_none(self):
        out = collect(self.proxy)
        self.assertEqual(
            sample(out, "docker_container_health", name="enigma-bbs1", status="healthy"), "1")
        # warez-server's inspect carries no Health block at all.
        self.assertEqual(
            sample(out, "docker_container_health", name="warez-server", status="none"), "1")

    def test_stats_are_requested_one_shot(self):
        """Without one-shot, Docker sleeps a second per container before
        answering and a dozen containers outlast the scrape interval."""
        collect(self.proxy)
        stats_calls = [p for p in FakeDocker.seen if "/stats" in p]
        self.assertTrue(stats_calls)
        for path in stats_calls:
            self.assertIn("one-shot=true", path)
            self.assertIn("stream=false", path)

    def test_docker_nanosecond_timestamps_parse(self):
        """Docker pads fractional seconds to 9 digits, which fromisoformat
        rejected before 3.11, and uses a trailing Z."""
        # 1786062141 cross-checked with `date -u -d '2026-08-07T00:22:21Z' +%s`.
        self.assertAlmostEqual(
            parse_started_at("2026-08-07T00:22:21.937457335Z") or 0,
            1786062141.937457, places=3)
        self.assertIsNone(parse_started_at("0001-01-01T00:00:00Z"))
        self.assertIsNone(parse_started_at(""))

    def test_api_failure_reports_down_rather_than_a_blank_page(self):
        FakeDocker.fail = True
        out = collect(self.proxy)
        self.assertEqual(sample(out, "docker_up"), "0")
        self.assertIsNone(sample(out, "docker_containers"))


if __name__ == "__main__":
    unittest.main()
