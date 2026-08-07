"""Prometheus exporter for per-container CPU, memory and network.

This exists because cAdvisor does not work on this host. Docker 29 here uses the
containerd image store ("Storage Driver: overlayfs"), and /var/lib/docker/image/
holds nothing but identity-cache.db -- there is no image/<driver>/layerdb tree.
cAdvisor v0.49 walks that tree to map a cgroup back to a container and fails on
every one:

    Failed to create existing container: /system.slice/docker-<id>.scope:
    failed to identify the read-write layer ID ...
    open /rootfs/var/lib/docker/image/overlayfs/layerdb/mounts/<id>/mount-id:
    no such file or directory

The result is a healthy-looking cAdvisor that reports exactly one series, the
root cgroup, and no container ever appears in a panel. Asking Docker for the
numbers instead sidesteps the whole layer-mapping problem.

It also never touches the Docker socket. Mounting /var/run/docker.sock into a
container is equivalent to handing it root on the host -- read-only on the mount
does not change that, because the socket is an API and the API can create
privileged containers. This talks TCP to a docker-socket-proxy that permits only
GET on /containers/*, so the blast radius of this exporter being compromised is
"it can list containers".

Stdlib-only, matching the other exporters here.

Metric shapes worth knowing:

  * Memory is published as a working set (usage minus inactive_file), which is
    what cAdvisor's container_memory_working_set_bytes means and what an OOM
    kill actually tracks. Raw `usage` includes reclaimable page cache and reads
    alarmingly high on a container that has merely touched a lot of files.

  * CPU is the cumulative nanosecond counter Docker already keeps, exported as
    seconds. Prometheus rates it. Docker's own "CPU %" needs two samples and a
    one-shot stats call deliberately does not provide the second one.

  * `stats?one-shot=true` matters: without it Docker samples twice a second
    apart before answering, and a dozen containers would take longer to scrape
    than the scrape interval.
"""
import argparse
import json
import os
import sys
import time
from typing import Any
from datetime import datetime
from http.client import HTTPConnection
from http.server import BaseHTTPRequestHandler, HTTPServer

DEFAULT_PROXY = "docker-socket-proxy:2375"
DEFAULT_PORT = 9105

# Docker API version pinned low enough to be widely accepted and high enough for
# one-shot stats (1.41+). The daemon negotiates anything at or below its own.
API = "v1.43"

# Container names are operator-controlled, not attacker-controlled, so this is
# not the unbounded-cardinality trap the fail2ban exporter guards against -- but
# a runaway compose loop could still mint hundreds, and a silently truncated list
# reads as "that is all the containers".
NAME_CAP = 100


def escape(value: object) -> str:
    """Escape a Prometheus label value (backslash, quote, newline)."""
    return str(value).replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n")


class DockerError(RuntimeError):
    """The Docker API could not be reached or returned an error."""


def api_get(proxy: str, path: str, timeout: float = 5.0) -> Any:
    """GET a Docker API path through the proxy and return the decoded JSON."""
    host, _, port = proxy.partition(":")
    conn = HTTPConnection(host, int(port or 2375), timeout=timeout)
    try:
        conn.request("GET", path, headers={"Host": "docker", "Accept": "application/json"})
        resp = conn.getresponse()
        body = resp.read()
        if resp.status != 200:
            raise DockerError("GET %s -> %d %s" % (path, resp.status, body[:120]))
        return json.loads(body)
    except (OSError, ValueError) as err:
        raise DockerError("GET %s: %s" % (path, err)) from err
    finally:
        conn.close()


def parse_started_at(value: str) -> float | None:
    """Docker's RFC3339 StartedAt to Unix time, or None if it never started.

    Docker pads fractional seconds to nanoseconds, which fromisoformat rejects
    before Python 3.11, and uses a trailing Z. Both are normalised here rather
    than depending on the interpreter version.
    """
    if not value or value.startswith("0001-01-01"):
        return None  # Docker's zero value for "never started"
    text = value.replace("Z", "+00:00")
    if "." in text:
        head, _, tail = text.partition(".")
        frac, sign, offset = (
            tail.partition("+") if "+" in tail else tail.partition("-")
        )
        text = "%s.%s%s%s" % (head, frac[:6], sign, offset)
    try:
        return datetime.fromisoformat(text).timestamp()
    except ValueError:
        return None


def container_name(entry: dict[str, Any]) -> str:
    """The display name Docker shows, without its leading slash."""
    names = entry.get("Names") or []
    if names:
        return names[0].lstrip("/")
    return (entry.get("Id") or "unknown")[:12]


def read_stats(proxy: str, cid: str) -> dict[str, float]:
    """CPU/memory/network/pids for one container, from a single-shot sample."""
    raw = api_get(
        proxy, "/%s/containers/%s/stats?stream=false&one-shot=true" % (API, cid)
    )
    out = {}

    cpu = (raw.get("cpu_stats") or {}).get("cpu_usage") or {}
    if "total_usage" in cpu:
        out["cpu_seconds"] = cpu["total_usage"] / 1e9

    mem = raw.get("memory_stats") or {}
    if "usage" in mem:
        inactive = (mem.get("stats") or {}).get("inactive_file", 0)
        out["memory"] = max(0, mem["usage"] - inactive)
    # memory_stats.limit is deliberately NOT used: for a container with no limit
    # Docker reports the host's total RAM there, and a "limit" line at 16 GiB
    # flattens a 10 MiB working set into the axis. The real limit comes from
    # HostConfig.Memory in inspect, where 0 means unlimited.

    rx = tx = None
    for iface in (raw.get("networks") or {}).values():
        rx = (rx or 0) + iface.get("rx_bytes", 0)
        tx = (tx or 0) + iface.get("tx_bytes", 0)
    if rx is not None:
        out["net_rx"], out["net_tx"] = rx, tx

    pids = (raw.get("pids_stats") or {}).get("current")
    if pids is not None:
        out["pids"] = pids
    return out


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


HELP = {
    "docker_container_running":
        "1 while the container is running. Stopped containers still report 0 "
        "rather than vanishing, so a board that dies is a falling line.",
    "docker_container_health":
        "The container's healthcheck verdict as a label. Only the current "
        "verdict is emitted; containers with no healthcheck report 'none'.",
    "docker_container_restarts":
        "Times Docker has restarted this container. A gauge, not a counter: it "
        "resets to zero when the container is recreated, which a counter would "
        "read as a reset and rate() would turn into imaginary restarts.",
    "docker_container_start_time_seconds":
        "Unix time the container last started; subtract from now() for uptime.",
    "docker_container_cpu_seconds_total":
        "Cumulative CPU seconds used by the container. Rate this rather than "
        "reading Docker's own CPU %, which needs two samples.",
    "docker_container_memory_bytes":
        "Working set: memory in use minus reclaimable page cache. This is what "
        "an OOM kill tracks; raw usage reads high on a container that has "
        "merely touched a lot of files.",
    "docker_container_memory_limit_bytes":
        "The container's configured memory limit. Absent entirely when the "
        "container is unlimited, rather than reported as the host's total RAM "
        "-- a limit line at host size flattens a small working set off the axis.",
    "docker_container_network_receive_bytes_total":
        "Bytes received across all the container's interfaces.",
    "docker_container_network_transmit_bytes_total":
        "Bytes transmitted across all the container's interfaces.",
    "docker_container_pids": "Processes running inside the container.",
}


def collect(proxy: str) -> str:
    """Render the exposition text for one scrape."""
    started = time.time()
    exp = Exposition()

    try:
        containers = api_get(proxy, "/%s/containers/json?all=true" % API)
    except DockerError as err:
        print("scrape failed: %s" % err, file=sys.stderr, flush=True)
        exp.add("docker_up", "gauge",
                "Whether the Docker API answered through the proxy.", 0)
        return exp.render()

    containers = sorted(containers, key=container_name)
    shown = containers[:NAME_CAP]

    for entry in shown:
        name = container_name(entry)
        cid = entry.get("Id", "")
        lbl = {"name": name}
        running = entry.get("State") == "running"

        exp.add("docker_container_running", "gauge", HELP["docker_container_running"],
                1 if running else 0, lbl)

        try:
            info = api_get(proxy, "/%s/containers/%s/json" % (API, cid))
        except DockerError:
            info = {}
        state = info.get("State") or {}

        exp.add("docker_container_health", "gauge", HELP["docker_container_health"], 1,
                {"name": name, "status": (state.get("Health") or {}).get("Status", "none")})
        # 0 in HostConfig.Memory means unlimited, and an unlimited container
        # gets no limit series at all.
        limit = ((info.get("HostConfig") or {}).get("Memory") or 0)
        if limit > 0:
            exp.add("docker_container_memory_limit_bytes", "gauge",
                    HELP["docker_container_memory_limit_bytes"], limit, lbl)
        if "RestartCount" in info:
            exp.add("docker_container_restarts", "gauge",
                    HELP["docker_container_restarts"], info["RestartCount"], lbl)
        start = parse_started_at(state.get("StartedAt", ""))
        if start is not None:
            exp.add("docker_container_start_time_seconds", "gauge",
                    HELP["docker_container_start_time_seconds"], "%.3f" % start, lbl)

        if not running:
            continue  # a stopped container has no stats to sample
        try:
            stats = read_stats(proxy, cid)
        except DockerError as err:
            print("stats for %s failed: %s" % (name, err), file=sys.stderr, flush=True)
            continue

        for key, metric, kind, fmt in (
            ("cpu_seconds", "docker_container_cpu_seconds_total", "counter", "%.3f"),
            ("memory", "docker_container_memory_bytes", "gauge", "%d"),
            ("net_rx", "docker_container_network_receive_bytes_total", "counter", "%d"),
            ("net_tx", "docker_container_network_transmit_bytes_total", "counter", "%d"),
            ("pids", "docker_container_pids", "gauge", "%d"),
        ):
            if key in stats:
                exp.add(metric, kind, HELP[metric], fmt % stats[key], lbl)

    exp.add("docker_containers", "gauge",
            "Containers Docker knows about, running or not.", len(containers))
    exp.add("docker_containers_truncated", "gauge",
            "Containers omitted because the per-scrape cap of %d was reached."
            % NAME_CAP, max(0, len(containers) - NAME_CAP))
    exp.add("docker_scrape_duration_seconds", "gauge",
            "Time taken to poll the Docker API for this scrape.",
            "%.6f" % (time.time() - started))
    exp.add("docker_up", "gauge",
            "Whether the Docker API answered through the proxy.", 1)
    return exp.render()


class Handler(BaseHTTPRequestHandler):
    proxy = DEFAULT_PROXY

    def do_GET(self):
        if self.path.split("?")[0] not in ("/metrics", "/"):
            self.send_error(404)
            return
        body = collect(self.proxy).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format, *args):  # noqa: A002 - signature is the base class's
        pass  # a scrape every 15s is not news


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--proxy", default=os.environ.get("DOCKER_PROXY", DEFAULT_PROXY))
    ap.add_argument("--port", type=int, default=int(os.environ.get("PORT", DEFAULT_PORT)))
    args = ap.parse_args()

    Handler.proxy = args.proxy
    print("docker exporter on :%d via %s" % (args.port, args.proxy), flush=True)
    HTTPServer(("", args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
