"""Prometheus exporter for fail2ban's ban state.

Reads fail2ban's SQLite database read-only and serves the exposition format on
/metrics. Nothing is written and the fail2ban socket is never touched, so this
runs unprivileged alongside the fail2ban container rather than inside it.

Python because it is the only runtime here that reads SQLite and speaks HTTP out
of its standard library. A Go exporter would need a SQLite driver in go.mod
(cgo, or the very large pure-Go one) and a Node one either a native module or a
newer base image — and this project avoids adding dependencies to either, for
the reasons in web/metrics.js. Stdlib-only keeps the sidecar to one file with
nothing to pin.
"""
import argparse
import os
import sqlite3
import sys
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

# The ceiling on how many banned IPs get their own time series in one scrape.
#
# An IP that reaches this exporter is by definition attacker-controlled, and a
# label built from one is the same unbounded-cardinality trap the gateway
# already refuses for dialled numbers: a subnet sweep would mint a series per
# source and Prometheus keeps every one of them for the whole retention window,
# long after the ban lapses. The count gauge is always exact; only the per-IP
# list is capped, newest bans first, and whatever it drops is published as
# telix_fail2ban_banned_ip_truncated rather than silently disappearing.
IP_CAP = 50

DEFAULT_DB = "/data/db/fail2ban.sqlite3"
DEFAULT_PORT = 9102


def escape(value):
    """Escape a Prometheus label value (backslash, quote, newline)."""
    return (
        str(value)
        .replace("\\", "\\\\")
        .replace('"', '\\"')
        .replace("\n", "\\n")
    )


def read_state(db_path, now):
    """Return (jails, live_bans, totals) from the database.

    live_bans is [(jail, ip, expiry)] newest ban first, where expiry is None for
    a permanent ban. Raises if the database cannot be read.
    """
    # Read-only URI so a corrupt or half-written database can never be modified
    # by a scrape, and so the volume can stay mounted :ro.
    uri = "file:%s?mode=ro" % db_path
    db = sqlite3.connect(uri, uri=True, timeout=5)
    try:
        jails = [r[0] for r in db.execute("SELECT name FROM jails ORDER BY name")]

        # bips is fail2ban's "currently banned" table, but it keeps rows after a
        # ban lapses — purging is lazy — so an unfiltered read reports IPs that
        # are free to connect again. bantime -1 means permanent, and must not be
        # run through the arithmetic at all.
        rows = db.execute(
            """
            SELECT jail, ip, timeofban, bantime
              FROM bips
             WHERE bantime < 0 OR timeofban + bantime > ?
             ORDER BY timeofban DESC
            """,
            (now,),
        ).fetchall()

        totals = dict(db.execute("SELECT jail, count(*) FROM bans GROUP BY jail"))
    finally:
        db.close()

    live = [
        (jail, ip, None if bantime < 0 else timeofban + bantime)
        for (jail, ip, timeofban, bantime) in rows
    ]
    return jails, live, totals


def collect(db_path, now=None):
    """Render the exposition text for one scrape."""
    now = int(now if now is not None else time.time())
    out = []

    try:
        jails, live, totals = read_state(db_path, now)
    except Exception as err:  # unreadable, locked, corrupt, absent
        print("scrape failed: %s" % err, file=sys.stderr, flush=True)
        out.append("# HELP telix_fail2ban_up Whether the fail2ban database could be read.")
        out.append("# TYPE telix_fail2ban_up gauge")
        out.append("telix_fail2ban_up 0")
        return "\n".join(out) + "\n"

    # Jails are pre-created at zero: an absent series makes a stat panel read
    # "No data" instead of 0, which looks like a broken exporter.
    counts = {jail: 0 for jail in jails}
    for jail, _ip, _expiry in live:
        counts[jail] = counts.get(jail, 0) + 1

    out.append("# HELP telix_fail2ban_banned IPs currently banned, per jail.")
    out.append("# TYPE telix_fail2ban_banned gauge")
    for jail in sorted(counts):
        out.append('telix_fail2ban_banned{jail="%s"} %d' % (escape(jail), counts[jail]))

    out.append("# HELP telix_fail2ban_banned_ip Unix time the ban on this IP expires "
               "(+Inf if permanent). Capped at %d series per scrape." % IP_CAP)
    out.append("# TYPE telix_fail2ban_banned_ip gauge")
    for jail, ip, expiry in live[:IP_CAP]:
        out.append(
            'telix_fail2ban_banned_ip{jail="%s",ip="%s"} %s'
            % (escape(jail), escape(ip), "+Inf" if expiry is None else str(expiry))
        )

    out.append("# HELP telix_fail2ban_banned_ip_truncated Banned IPs omitted from "
               "telix_fail2ban_banned_ip because the per-scrape cap was reached.")
    out.append("# TYPE telix_fail2ban_banned_ip_truncated gauge")
    out.append("telix_fail2ban_banned_ip_truncated %d" % max(0, len(live) - IP_CAP))

    # A gauge, not a counter, and named to say so. fail2ban's bans table looks
    # like an append-only history but `fail2ban-client unban` deletes the row
    # from it as well as from bips — measured, both tables drop to zero — and
    # dbpurgeage ages rows out besides. Published as a counter, every unban
    # would read to Prometheus as a counter reset and rate() would invent
    # traffic that never happened.
    out.append("# HELP telix_fail2ban_bans_recorded Ban records fail2ban is currently "
               "holding. Falls when an IP is unbanned or a record is purged, so it is "
               "a gauge and not a cumulative total.")
    out.append("# TYPE telix_fail2ban_bans_recorded gauge")
    for jail in sorted(set(jails) | set(totals)):
        out.append('telix_fail2ban_bans_recorded{jail="%s"} %d'
                   % (escape(jail), totals.get(jail, 0)))

    out.append("# HELP telix_fail2ban_up Whether the fail2ban database could be read.")
    out.append("# TYPE telix_fail2ban_up gauge")
    out.append("telix_fail2ban_up 1")

    return "\n".join(out) + "\n"


class Handler(BaseHTTPRequestHandler):
    db_path = DEFAULT_DB

    def do_GET(self):
        if self.path.split("?")[0] not in ("/metrics", "/"):
            self.send_error(404)
            return
        body = collect(self.db_path).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format, *args):  # noqa: A002 - signature is the base class's
        pass  # a scrape every 15s is not news


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--db", default=os.environ.get("FAIL2BAN_DB", DEFAULT_DB))
    ap.add_argument("--port", type=int, default=int(os.environ.get("PORT", DEFAULT_PORT)))
    args = ap.parse_args()

    Handler.db_path = args.db
    print("fail2ban exporter on :%d reading %s" % (args.port, args.db), flush=True)
    HTTPServer(("", args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
