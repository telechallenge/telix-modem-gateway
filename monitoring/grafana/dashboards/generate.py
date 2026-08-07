#!/usr/bin/env python3
"""Generate the committed Grafana dashboard JSON.

Run after editing this file, and commit the result:

    python3 monitoring/grafana/dashboards/generate.py

The JSON files next to this script are what Grafana provisions (read-only, see
monitoring/grafana/provisioning/dashboards/telix.yml) -- this script is where
they are *edited*. Three of the six dashboards are the same board dashboard
pointed at a different instance and two more are the same container dashboard;
hand-maintaining five near-identical 700-line JSON documents guarantees they
drift, and a panel fixed on one board but not the other two is worse than no
panel at all.

telix.json is NOT generated here. It predates this script and stays hand-written.

Two things carried over from the Telix dashboard's history, both worth keeping:

  * The datasource uid is the literal string "prometheus", matching the fixed uid
    in the provisioned datasource. A provisioned dashboard has no import step, so
    a ${DS_PROMETHEUS} placeholder would never be substituted and every panel
    would fail to resolve its datasource.

  * A count is `round(increase(m[$__range]))`, never `rate(m[...]) * 3600`. The
    gateway dashboard shipped the latter under a title promising a count, and on
    a board taking a handful of calls a day it rendered two real dials as 240.
    round() is not cosmetic either: increase() extrapolates to the window edges
    and returns 2.02 for two calls.
"""
import json
import os

DS = {"type": "prometheus", "uid": "prometheus"}
HERE = os.path.dirname(os.path.abspath(__file__))

# (instance id as the exporter labels it, container name, board name)
ENIGMA_BOARDS = [
    ("bbs1", "enigma-bbs1", "Fungus Land"),
    ("bbs2", "enigma-bbs2", "Happy Friends BBS"),
    ("bbs3", "enigma-bbs3", "HPACV Heaven"),
]

# Boards with no metrics of their own: not ENiGMA, no shared SQLite schema, no
# exposition endpoint. The Docker API is all there is, so these dashboards say
# plainly that they are container-level only.
CONTAINER_BOARDS = [
    ("boringland", "boringland_filedrop-bbs-1", "Boringland Filedrop",
     "A Python filedrop BBS on port 2475."),
    ("warez-server", "warez-server", "Warez Server",
     "A telnet BBS on port 2300."),
]


def target(expr, legend=None, ref="A", instant=False, fmt=None):
    t = {"refId": ref, "expr": expr}
    if legend:
        t["legendFormat"] = legend
    if instant:
        t["instant"] = True
    if fmt:
        t["format"] = fmt
    return t


def targets(*exprs):
    """[(expr, legend)] -> numbered refIds."""
    out = []
    for i, item in enumerate(exprs):
        expr, legend = item if isinstance(item, tuple) else (item, None)
        out.append(target(expr, legend, ref=chr(ord("A") + i)))
    return out


def row(title, y):
    return {
        "type": "row", "title": title, "collapsed": False,
        "gridPos": {"h": 1, "w": 24, "x": 0, "y": y}, "panels": [],
    }


def stat(title, expr, x, y, w=3, h=4, unit="short", desc="", mappings=None,
         color="text", decimals=None, thresholds=None):
    field = {"unit": unit, "mappings": mappings or [],
             "thresholds": thresholds or {"mode": "absolute",
                                          "steps": [{"color": color, "value": None}]}}
    if decimals is not None:
        field["decimals"] = decimals
    return {
        "type": "stat", "title": title, "description": desc,
        "gridPos": {"h": h, "w": w, "x": x, "y": y}, "datasource": DS,
        "targets": [target(expr, instant=True)],
        "fieldConfig": {"defaults": field, "overrides": []},
        "options": {
            "reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
            "textMode": "auto", "colorMode": "value", "graphMode": "none",
            "justifyMode": "auto",
        },
    }


def timeseries(title, exprs, x, y, w=12, h=8, unit="short", desc="",
               stacked=False, style="line", fill=10, min_=0, soft_max=None):
    # axisSoftMax floors the axis without capping it. Grafana scales an
    # all-zero series to 0-100 in raw units, which on a percentunit panel reads
    # "10000%" above a flat line -- an idle container looks like a broken panel.
    custom_extra = {"axisSoftMax": soft_max} if soft_max is not None else {}
    return {
        "type": "timeseries", "title": title, "description": desc,
        "gridPos": {"h": h, "w": w, "x": x, "y": y}, "datasource": DS,
        "targets": targets(*exprs),
        "fieldConfig": {
            "defaults": {
                "unit": unit, "min": min_,
                "custom": dict({
                    "drawStyle": style, "lineWidth": 2, "fillOpacity": fill,
                    "showPoints": "never",
                    "stacking": {"mode": "normal" if stacked else "none"},
                }, **custom_extra),
            },
            "overrides": [],
        },
        "options": {
            "legend": {"displayMode": "list", "placement": "bottom", "showLegend": True},
            "tooltip": {"mode": "multi", "sort": "desc"},
        },
    }


def bargauge(title, exprs, x, y, w=8, h=7, unit="short", desc=""):
    return {
        "type": "bargauge", "title": title, "description": desc,
        "gridPos": {"h": h, "w": w, "x": x, "y": y}, "datasource": DS,
        "targets": [target(e, l, ref=chr(ord("A") + i), instant=True)
                    for i, (e, l) in enumerate(exprs)],
        "fieldConfig": {
            "defaults": {"unit": unit, "min": 0,
                         "thresholds": {"mode": "absolute",
                                        "steps": [{"color": "blue", "value": None}]}},
            "overrides": [],
        },
        "options": {
            "displayMode": "gradient", "orientation": "horizontal",
            "reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
            "showUnfilled": True,
        },
    }


UP_DOWN = [{
    "type": "value",
    "options": {
        "0": {"text": "DOWN", "color": "red", "index": 0},
        "1": {"text": "UP", "color": "green", "index": 1},
    },
}]


def dashboard(uid, title, tags, panels, description=""):
    return {
        "uid": uid, "title": title, "description": description, "tags": tags,
        "schemaVersion": 39, "version": 1, "editable": True,
        "refresh": "30s", "timezone": "", "time": {"from": "now-6h", "to": "now"},
        "templating": {"list": []},
        "annotations": {"list": []},
        "panels": panels,
    }


def container_panels(container, y, note=""):
    """The container resource row every board dashboard ends with.

    Sourced from monitoring/docker-exporter, not cAdvisor -- see that file's
    docstring for why cAdvisor reports nothing on this host.
    """
    sel = '{name="%s"}' % container
    return [
        row("Container resources" + (" \u2014 " + note if note else ""), y),
        timeseries(
            "CPU",
            [('rate(docker_container_cpu_seconds_total%s[$__rate_interval])' % sel,
              container)],
            0, y + 1, w=8, h=7, unit="percentunit", soft_max=1,
            desc="Share of one CPU core, rated from Docker's cumulative counter. "
                 "1.0 is one core fully busy; the compose limit for an ENiGMA "
                 "board is 1.0.",
        ),
        timeseries(
            "Memory", [
                ('docker_container_memory_bytes%s' % sel, "working set"),
                ('docker_container_memory_limit_bytes%s' % sel, "limit"),
            ],
            8, y + 1, w=8, h=7, unit="bytes",
            desc="Working set (usage minus reclaimable page cache) against the "
                 "container's limit. This is what an OOM kill tracks -- raw usage "
                 "reads high on a container that has merely touched a lot of files.",
        ),
        timeseries(
            "Network", [
                ('rate(docker_container_network_receive_bytes_total%s[$__rate_interval])' % sel,
                 "receive"),
                ('rate(docker_container_network_transmit_bytes_total%s[$__rate_interval])' % sel,
                 "transmit"),
            ],
            16, y + 1, w=8, h=7, unit="Bps",
            desc="Traffic in and out of the container, summed across its "
                 "interfaces. Callers included.",
        ),
    ]


def enigma_dashboard(bbs, container, board):
    sel = '{bbs="%s"}' % bbs
    panels = [
        row("Board", 0),
        stat("Status", "enigma_up%s" % sel, 0, 1,
             mappings=UP_DOWN,
             desc="Whether the exporter could read this board's databases. Note "
                  "this is not a telnet reachability check -- it says the data is "
                  "readable, not that callers can get in."),
        stat("Users", "enigma_users%s" % sel, 3, 1),
        stat("Calls (24h)", "enigma_logins_last_24h%s" % sel, 6, 1,
             desc="Read from ENiGMA's login history log, which the BBS trims on "
                  "write -- so this is accurate for 24h but is not a lifetime total."),
        stat("Callers (24h)", "enigma_callers_last_24h%s" % sel, 9, 1,
             desc="Distinct users, so a caller who dialled six times counts once."),
        stat("Calls (all time)", "enigma_logins_total%s" % sel, 12, 1),
        stat("Files", "enigma_files%s" % sel, 15, 1),
        stat("File base", "enigma_file_bytes%s" % sel, 18, 1, unit="bytes"),
        stat("Last call", "enigma_last_login_timestamp_seconds%s * 1000" % sel,
             21, 1, unit="dateTimeFromNow",
             desc="Blank until somebody calls: the exporter omits the series "
                  "rather than reporting zero, which would plot as 1970."),

        row("Activity", 5),
        timeseries(
            "Calls per hour",
            [("round(increase(enigma_logins_total%s[1h]))" % sel, "calls")],
            0, 6, w=12, style="bars", fill=60,
            desc="A count of calls in each hour, not a rate scaled up to look "
                 "like one. round() matters: increase() extrapolates to the "
                 "window edges and would report 2.02 for two calls.",
        ),
        timeseries(
            "Population",
            [("enigma_users%s" % sel, "users"),
             ("enigma_messages%s" % sel, "messages"),
             ("enigma_files%s" % sel, "files")],
            12, 6, w=12,
            desc="Everything the board is accumulating, on one axis.",
        ),
        bargauge(
            "Files by area",
            [("enigma_files_by_area%s" % sel, "{{area}}")],
            0, 14, w=8,
            desc="Areas are capped at 64 per scrape; anything dropped shows up in "
                 "enigma_areas_truncated rather than vanishing silently.",
        ),
        bargauge(
            "Messages by area",
            [("enigma_messages_by_area%s" % sel, "{{area}}")],
            8, 14, w=8,
        ),
        timeseries(
            "File base",
            [("enigma_file_bytes%s" % sel, "bytes held"),
             ("enigma_upload_bytes_total%s" % sel, "uploaded, all time")],
            16, 14, w=8, h=7, unit="bytes",
            desc="Held vs uploaded diverge as files are pruned: the first is what "
                 "is on disk now, the second a lifetime counter.",
        ),

        row("Users", 21),
        timeseries(
            "Sessions and uploads per hour",
            [("round(increase(enigma_uploads_total%s[1h]))" % sel, "uploads"),
             ('enigma_user_events_recorded{bbs="%s",event="login"}' % bbs,
              "logins held in log")],
            0, 22, w=12, h=7,
            desc="Uploads is a true count per hour. The login series is a gauge "
                 "over a log ENiGMA trims, so it falls on its own -- it is here "
                 "to show the log's depth, not to be rated.",
        ),
        bargauge(
            "Accounts by status",
            [("enigma_users_by_status%s" % sel, "status {{status}}")],
            12, 22, w=6, h=7,
            desc="ENiGMA account_status: 0 disabled, 1 inactive, 2 active, 3 locked.",
        ),
        stat("Time online (all users)",
             "enigma_minutes_online_total%s * 60" % sel, 18, 22, w=3, h=7,
             unit="s"),
        stat("Achievement points",
             "enigma_achievement_points_total%s" % sel, 21, 22, w=3, h=7),
    ]
    panels += container_panels(container, 29)
    return dashboard(
        "enigma-%s" % bbs,
        "BBS — %s" % board,
        ["bbs", "enigma", bbs],
        panels,
        description="ENiGMA1/2 instance %s (container %s). Board statistics come "
                    "from the BBS's own SQLite databases, read by "
                    "monitoring/bbs-exporter; the resource row comes from cAdvisor."
                    % (bbs, container),
    )


def container_dashboard(slug, container, title, blurb):
    sel = '{name="%s"}' % container
    panels = [
        row("Container", 0),
        stat("Running", "docker_container_running%s" % sel, 0, 1, mappings=UP_DOWN,
             desc="Whether the container is up. This board exposes no metrics of "
                  "its own, so container liveness is the real signal -- it does "
                  "not prove callers can log in."),
        stat("Running since",
             "docker_container_start_time_seconds%s * 1000" % sel, 3, 1, w=4,
             unit="dateTimeFromNow",
             desc="Jumps to 'a few seconds ago' on every restart."),
        stat("Memory", "docker_container_memory_bytes%s" % sel, 7, 1, w=3,
             unit="bytes"),
        stat("CPU",
             "rate(docker_container_cpu_seconds_total%s[$__rate_interval])" % sel,
             10, 1, w=3, unit="percentunit"),
        stat("Processes", "docker_container_pids%s" % sel, 13, 1, w=3),
        stat("Restarts", "docker_container_restarts%s" % sel, 16, 1, w=3,
             desc="Resets to zero when the container is recreated, so this is a "
                  "gauge -- a rising value means it is crash-looping."),
    ]
    panels += container_panels(container, 5)
    return dashboard(
        "bbs-%s" % slug,
        "BBS — %s" % title,
        ["bbs", "container", slug],
        panels,
        description="%s Container-level only: this board is not ENiGMA, exposes no "
                    "metrics endpoint and shares no database schema, so cAdvisor is "
                    "the whole picture. Callers, files and messages are not "
                    "observable from outside it." % blurb,
    )


def virtualbox_dashboard():
    panels = [
        row("Fleet", 0),
        stat("Exporter", 'up{job="virtualbox"}', 0, 1, mappings=UP_DOWN,
             desc="Down means the systemd --user unit is not running "
                  "(systemctl --user status telix-vbox-exporter), NOT that the "
                  "VMs are gone."),
        stat("VMs registered", "vbox_vms_registered", 3, 1),
        stat("VMs running", "vbox_vms_running", 6, 1),
        stat("Hypervisor memory", "vbox_host_vmm_memory_bytes", 9, 1, w=4,
             unit="bytes",
             desc="Physical memory VirtualBox itself is holding, across all VMs."),
        stat("Host memory in use", "vbox_host_memory_used_bytes", 13, 1, w=4,
             unit="bytes", desc="VirtualBox's view of host memory."),

        row("Per VM", 5),
        {
            "type": "state-timeline", "title": "Power state",
            "description": "A registered-but-stopped VM reports 0 rather than "
                           "disappearing, so a VM that dies is a visible fall and "
                           "not an absent series.",
            "gridPos": {"h": 6, "w": 24, "x": 0, "y": 6}, "datasource": DS,
            "targets": [target("vbox_vm_running", "{{vm}}")],
            "fieldConfig": {
                "defaults": {"unit": "short", "mappings": [{
                    "type": "value",
                    "options": {
                        "0": {"text": "stopped", "color": "red", "index": 0},
                        "1": {"text": "running", "color": "green", "index": 1},
                    },
                }], "custom": {"lineWidth": 0, "fillOpacity": 90}},
                "overrides": [],
            },
            "options": {"showValue": "never", "mergeValues": True,
                        "legend": {"displayMode": "list", "placement": "bottom",
                                   "showLegend": True}},
        },
        timeseries(
            "CPU per VM",
            [("sum by (vm) (rate(vbox_vm_cpu_seconds_total[$__rate_interval]))",
              "{{vm}}")],
            0, 12, w=12, unit="percentunit", soft_max=1,
            desc="Rated from a monotonic counter read out of /proc, rather than "
                 "from VirtualBox's own pre-averaged percentage. 1.0 is one host "
                 "core fully busy. This is the VM process as the host sees it — "
                 "nothing here comes from inside the guest.",
        ),
        timeseries(
            "Memory per VM",
            [("vbox_vm_memory_resident_bytes", "{{vm}} resident"),
             ("vbox_vm_configured_memory_bytes", "{{vm}} configured")],
            12, 12, w=12, unit="bytes",
            desc="Resident is what the VM process actually occupies on the host; "
                 "configured is what the VM was given. A VM that has not touched "
                 "all its RAM sits well under its configured size.",
        ),
        bargauge(
            "Disk per VM", [("vbox_vm_disk_bytes", "{{vm}}")],
            0, 20, w=8, unit="bytes",
            desc="Actual size of all the VM's virtual disks combined.",
        ),
        timeseries(
            "Threads per VM", [("vbox_vm_threads", "{{vm}}")],
            8, 20, w=8, h=7,
            desc="From /proc. A VM that has wedged often shows a flat thread count "
                 "alongside flat CPU.",
        ),
        bargauge(
            "Uptime", [("time() - vbox_vm_start_time_seconds", "{{vm}}")],
            16, 20, w=8, unit="s",
            desc="Time since the VM process started. Resets on every power cycle.",
        ),
        {
            "type": "table", "title": "Inventory",
            "description": "Every registered VM, running or not, with the guest OS "
                           "VirtualBox has recorded for it.",
            "gridPos": {"h": 7, "w": 24, "x": 0, "y": 27}, "datasource": DS,
            "targets": [
                target("vbox_vm_info", ref="A", instant=True, fmt="table"),
                target("vbox_vm_configured_vcpus", ref="B", instant=True, fmt="table"),
                target("vbox_vm_configured_memory_bytes", ref="C", instant=True,
                       fmt="table"),
            ],
            "transformations": [
                {"id": "joinByField", "options": {"byField": "vm", "mode": "outer"}},
                {"id": "organize", "options": {
                    "excludeByName": {
                        "Time": True, "Time 1": True, "Time 2": True, "Time 3": True,
                        "__name__": True, "__name__ 1": True, "__name__ 2": True,
                        "__name__ 3": True, "job": True, "job 1": True, "job 2": True,
                        "job 3": True, "instance": True, "instance 1": True,
                        "instance 2": True, "instance 3": True, "service": True,
                        "service 1": True, "service 2": True, "service 3": True,
                        "Value #A": True,
                    },
                    "renameByName": {
                        "vm": "VM", "uuid": "UUID", "ostype": "Guest OS",
                        "Value #B": "vCPUs", "Value #C": "RAM",
                    },
                }},
            ],
            "fieldConfig": {
                "defaults": {},
                "overrides": [{
                    "matcher": {"id": "byName", "options": "RAM"},
                    "properties": [{"id": "unit", "value": "bytes"}],
                }],
            },
            "options": {"showHeader": True},
        },
    ]
    return dashboard(
        "virtualbox", "VirtualBox VMs", ["vms", "virtualbox"], panels,
        description="VirtualBox VMs measured entirely from the host: VBoxManage's "
                    "view plus the VBoxHeadless process in /proc. Nothing here is "
                    "reported by the guest OS, so it works the same for the DOS VM "
                    "as for Windows XP and needs no Guest Additions.",
    )


def main():
    written = []
    for bbs, container, board in ENIGMA_BOARDS:
        written.append(("enigma-%s.json" % bbs, enigma_dashboard(bbs, container, board)))
    for slug, container, title, blurb in CONTAINER_BOARDS:
        written.append(("%s.json" % slug, container_dashboard(slug, container, title, blurb)))
    written.append(("virtualbox.json", virtualbox_dashboard()))

    for name, doc in written:
        path = os.path.join(HERE, name)
        with open(path, "w") as fh:
            json.dump(doc, fh, indent=2)
            fh.write("\n")
        print("wrote %s (%d panels)" % (name, len(doc["panels"])))


if __name__ == "__main__":
    main()
