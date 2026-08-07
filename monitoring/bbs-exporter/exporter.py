"""Prometheus exporter for ENiGMA1/2 BBS instances.

Reads each instance's SQLite databases read-only off the bind-mounted data
directory and serves the exposition format on /metrics. Nothing is written and
the BBS process is never signalled, so this runs alongside the boards rather
than inside them and cannot affect a live session.

Stdlib-only for the same reason as monitoring/fail2ban-exporter: sqlite3 and
http.server both ship with CPython, so the sidecar is one file with nothing to
pin and no lockfile to keep in step.

Instances are discovered rather than configured. Any immediate subdirectory of
--root holding a db/ directory is an instance, keyed by its directory name
(bbs1, bbs2, ...), so standing up a fourth board needs no change here.

Two traps this file exists to avoid, both measured against the live boards:

  * ENiGMA runs its databases in WAL mode, and a WAL database opened with
    SQLite's immutable=1 URI silently returns *stale* data -- it reads the main
    database file and ignores the -wal alongside it. On a live board that read
    12 users where mode=ro read the true 15. Only mode=ro is used here, and
    there is deliberately no immutable fallback: reporting a wrong number
    confidently is worse than reporting the board down.

  * system_event_log and user_event_log are not append-only. ENiGMA trims both
    with DELETE (core/stat_log.js: appendSystemLogEntry honours a keep count or
    age, appendUserLogEntry a keepDays), so a row count over either table falls
    on its own. Published as a counter, every trim would read to Prometheus as a
    counter reset and rate() would invent activity that never happened -- the
    same trap monitoring/fail2ban-exporter documents for the bans table. They
    are *_recorded gauges. Only the system_stat values are true counters:
    incrementSystemStat is a read-add-write on a persistent property and never
    goes down.
"""
import argparse
import json
import os
import re
import sqlite3
import sys
import time
from datetime import datetime
from typing import Any
from http.server import BaseHTTPRequestHandler, HTTPServer

# The ceiling on how many message/file areas get their own time series per
# instance. Area tags come from the sysop's config rather than from callers, so
# this is not the attacker-controlled cardinality problem fail2ban's IP label
# has -- but a misgenerated config could still mint hundreds, and a silent
# truncation would read as "that's all the areas" when it is not. Whatever the
# cap drops is published as enigma_areas_truncated.
AREA_CAP = 64

# boardName in config.hjson, which is HJSON: the value may be bare
# (`boardName: Fungus Land`) or quoted, and either form may carry a trailing
# comma. Anything unparseable falls back to the directory name -- a board with
# an odd config should cost itself a nice title, not the whole scrape.
BOARD_NAME_RE = re.compile(r"^\s*\"?boardName\"?\s*:\s*(.+?)\s*,?\s*$", re.MULTILINE)

DEFAULT_ROOT = "/instances"
DEFAULT_PORT = 9104

# Databases read per instance, and the metric-friendly name each is published
# under in enigma_db_size_bytes.
DATABASES = ("system", "user", "message", "file")


def escape(value: object) -> str:
    """Escape a Prometheus label value (backslash, quote, newline)."""
    return str(value).replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n")


def parse_timestamp(value: object) -> float | None:
    """ENiGMA's ISO-8601-with-offset timestamps to a Unix time, or None.

    Stored as e.g. '2026-08-04T07:07:33.173+00:00'. Returns None rather than
    raising: a board that has never been called has no timestamps at all, and
    that is a normal state, not a scrape failure.
    """
    if not value:
        return None
    try:
        return datetime.fromisoformat(str(value)).timestamp()
    except (TypeError, ValueError):
        return None


def connect(db_path: str) -> sqlite3.Connection:
    """Open a database read-only. See the module docstring on immutable=1."""
    return sqlite3.connect("file:%s?mode=ro" % db_path, uri=True, timeout=5)


def _scalar(db: sqlite3.Connection, sql: str, args: tuple[object, ...] = ()) -> Any:
    """First column of the first row, or None when there is no row at all.

    Every aggregate here runs against boards that may be freshly installed and
    completely empty, where sum() and max() return NULL.
    """
    row = db.execute(sql, args).fetchone()
    return None if not row else row[0]


def _int(db: sqlite3.Connection, sql: str, args: tuple[object, ...] = ()) -> int:
    """_scalar as an int, with NULL and no-rows folded to zero."""
    value = _scalar(db, sql, args)
    return 0 if value is None else int(value)


def board_name(inst_path: str, fallback: str) -> str:
    """boardName from config.hjson, or the directory name if unreadable."""
    try:
        with open(os.path.join(inst_path, "config", "config.hjson"), "rb") as fh:
            text = fh.read().decode("utf-8", "replace")
    except OSError:
        return fallback
    match = BOARD_NAME_RE.search(text)
    if not match:
        return fallback
    return match.group(1).strip().strip('"').strip("'") or fallback


def discover(root: str) -> list[tuple[str, str]]:
    """[(instance_id, path)] for every subdirectory of root holding a db/."""
    try:
        entries = sorted(os.listdir(root))
    except OSError as err:
        print("cannot list %s: %s" % (root, err), file=sys.stderr, flush=True)
        return []
    found = []
    for name in entries:
        path = os.path.join(root, name)
        if os.path.isdir(os.path.join(path, "db")):
            found.append((name, path))
    return found


def read_system(path: str, now: int, stats: dict[str, Any]) -> None:
    """system.sqlite3: cumulative counters plus the trimmed event logs."""
    with connect(os.path.join(path, "db", "system.sqlite3")) as db:
        # The only genuinely monotonic values ENiGMA keeps. Stored as text.
        for name in ("login_count", "ul_total_count", "ul_total_bytes"):
            stats[name] = _int(
                db,
                "SELECT CAST(stat_value AS INTEGER) FROM system_stat "
                "WHERE stat_name = ?",
                (name,),
            )

        # Trimmed tables -- gauges. See the module docstring.
        stats["user_events"] = dict(
            db.execute(
                "SELECT log_name, count(*) FROM user_event_log GROUP BY log_name"
            ).fetchall()
        )
        stats["login_history"] = _int(
            db,
            "SELECT count(*) FROM system_event_log WHERE log_name = 'user_login_history'",
        )

        # Calls inside the last 24h. String comparison is exact here because the
        # stored format is fixed-width ISO-8601 with an explicit offset, and
        # every row is written by the same process with the same offset.
        cutoff = datetime.fromtimestamp(now - 86400).astimezone().isoformat()
        stats["logins_24h"] = _int(
            db,
            "SELECT count(*) FROM system_event_log "
            "WHERE log_name = 'user_login_history' AND timestamp > ?",
            (cutoff,),
        )

        # Distinct callers in the last 24h, out of the JSON log_value.
        rows = db.execute(
            "SELECT log_value FROM system_event_log "
            "WHERE log_name = 'user_login_history' AND timestamp > ?",
            (cutoff,),
        ).fetchall()
    callers = set()
    for (value,) in rows:
        try:
            callers.add(json.loads(value).get("userId"))
        except (TypeError, ValueError):
            continue
    stats["callers_24h"] = len(callers - {None})


def read_users(path: str, stats: dict[str, Any]) -> None:
    """user.sqlite3: population, account status mix, time online."""
    with connect(os.path.join(path, "db", "user.sqlite3")) as db:
        stats["users"] = _int(db, "SELECT count(*) FROM user")
        stats["users_by_status"] = dict(
            db.execute(
                "SELECT prop_value, count(*) FROM user_property "
                "WHERE prop_name = 'account_status' GROUP BY prop_value"
            ).fetchall()
        )
        for metric, prop in (
            ("minutes_online", "minutes_online_total_count"),
            ("achievement_points", "achievement_total_points"),
        ):
            stats[metric] = _int(
                db,
                "SELECT sum(CAST(prop_value AS INTEGER)) FROM user_property "
                "WHERE prop_name = ?",
                (prop,),
            )
        stats["last_login"] = parse_timestamp(
            _scalar(
                db,
                "SELECT max(prop_value) FROM user_property "
                "WHERE prop_name = 'last_login_timestamp'",
            )
        )


def read_messages(path: str, stats: dict[str, Any]) -> None:
    """message.sqlite3: totals, per-area counts, most recent post."""
    with connect(os.path.join(path, "db", "message.sqlite3")) as db:
        stats["messages"] = _int(db, "SELECT count(*) FROM message")
        stats["messages_by_area"] = db.execute(
            "SELECT area_tag, count(*) FROM message GROUP BY area_tag "
            "ORDER BY count(*) DESC"
        ).fetchall()
        stats["last_message"] = parse_timestamp(
            _scalar(db, "SELECT max(modified_timestamp) FROM message")
        )


def read_files(path: str, stats: dict[str, Any]) -> None:
    """file.sqlite3: totals, per-area counts, bytes and downloads."""
    with connect(os.path.join(path, "db", "file.sqlite3")) as db:
        stats["files"] = _int(db, "SELECT count(*) FROM file")
        stats["files_by_area"] = db.execute(
            "SELECT area_tag, count(*) FROM file GROUP BY area_tag "
            "ORDER BY count(*) DESC"
        ).fetchall()
        # meta_value is VARCHAR for every key, so both of these must be CAST or
        # SQLite sums them as zero.
        stats["file_bytes"] = _int(
            db,
            "SELECT sum(CAST(meta_value AS INTEGER)) FROM file_meta "
            "WHERE meta_name = 'byte_size'",
        )
        stats["downloads"] = _int(
            db,
            "SELECT sum(CAST(meta_value AS INTEGER)) FROM file_meta "
            "WHERE meta_name = 'dl_count'",
        )
        stats["last_upload"] = parse_timestamp(
            _scalar(db, "SELECT max(upload_timestamp) FROM file")
        )


def db_sizes(path: str) -> dict[str, int]:
    """{db_name: bytes} including the -wal, which is most of the size on disk."""
    sizes = {}
    for name in DATABASES:
        total = 0
        for suffix in ("", "-wal"):
            try:
                total += os.path.getsize(
                    os.path.join(path, "db", "%s.sqlite3%s" % (name, suffix))
                )
            except OSError:
                pass
        sizes[name] = total
    return sizes


def read_instance(path: str, now: int) -> dict[str, Any]:
    """Every stat for one instance. Raises if any database cannot be read."""
    stats = {}
    read_system(path, now, stats)
    read_users(path, stats)
    read_messages(path, stats)
    read_files(path, stats)
    stats["db_sizes"] = db_sizes(path)
    return stats


class Exposition:
    """Accumulates samples so each metric family emits its HELP/TYPE once."""

    def __init__(self) -> None:
        self._families = []
        self._index = {}

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


def _emit_areas(exp: "Exposition", bbs: str, kind: str, rows: list[tuple[str, int]]) -> None:
    """Per-area counts under the cardinality cap, reporting what it dropped."""
    for area, count in rows[:AREA_CAP]:
        exp.add(
            "enigma_%s_by_area" % kind,
            "gauge",
            "%s held in each area." % kind.capitalize(),
            count,
            {"bbs": bbs, "area": area},
        )
    exp.add(
        "enigma_areas_truncated",
        "gauge",
        "Areas omitted from the per-area series because the per-scrape cap of "
        "%d was reached." % AREA_CAP,
        max(0, len(rows) - AREA_CAP),
        {"bbs": bbs, "kind": kind},
    )


def _emit_instance(exp: "Exposition", bbs: str, board: str, stats: dict[str, Any]) -> None:
    """Every metric family for one successfully-read instance."""
    lbl = {"bbs": bbs}

    exp.add("enigma_up", "gauge", "Whether this board's databases could be read.", 1, lbl)

    # Cumulative system counters. incrementSystemStat is read-add-write on a
    # persistent property, so these only ever rise -- genuine counters.
    for name, help_text in (
        ("logins", "Calls answered since the board was installed."),
        ("uploads", "Files uploaded since the board was installed."),
    ):
        exp.add(
            "enigma_%s_total" % name,
            "counter",
            help_text,
            stats["login_count" if name == "logins" else "ul_total_count"],
            lbl,
        )
    exp.add(
        "enigma_upload_bytes_total",
        "counter",
        "Bytes uploaded since the board was installed.",
        stats["ul_total_bytes"],
        lbl,
    )

    # Trimmed logs -- gauges, and named to say so.
    for event, count in sorted(stats["user_events"].items()):
        exp.add(
            "enigma_user_events_recorded",
            "gauge",
            "Rows ENiGMA is currently holding in user_event_log, per event. "
            "Falls when the log is trimmed, so it is a gauge and not a "
            "cumulative total.",
            count,
            {"bbs": bbs, "event": event},
        )
    exp.add(
        "enigma_login_history_recorded",
        "gauge",
        "Rows ENiGMA is currently holding in the user_login_history system log. "
        "Trimmed on write, so a gauge and not a cumulative total.",
        stats["login_history"],
        lbl,
    )
    exp.add(
        "enigma_logins_last_24h",
        "gauge",
        "Calls answered in the last 24 hours, read out of the (trimmed) login "
        "history log.",
        stats["logins_24h"],
        lbl,
    )
    exp.add(
        "enigma_callers_last_24h",
        "gauge",
        "Distinct users who called in the last 24 hours.",
        stats["callers_24h"],
        lbl,
    )

    exp.add("enigma_users", "gauge", "User accounts on the board.", stats["users"], lbl)
    for status, count in sorted(stats["users_by_status"].items()):
        exp.add(
            "enigma_users_by_status",
            "gauge",
            "User accounts by ENiGMA account_status "
            "(0 disabled, 1 inactive, 2 active, 3 locked).",
            count,
            {"bbs": bbs, "status": status},
        )
    exp.add(
        "enigma_minutes_online_total",
        "counter",
        "Minutes users have spent online, summed across all accounts.",
        stats["minutes_online"],
        lbl,
    )
    exp.add(
        "enigma_achievement_points_total",
        "counter",
        "Achievement points earned, summed across all accounts.",
        stats["achievement_points"],
        lbl,
    )

    exp.add("enigma_messages", "gauge", "Messages held on the board.", stats["messages"], lbl)
    _emit_areas(exp, bbs, "messages", stats["messages_by_area"])
    exp.add("enigma_files", "gauge", "Files held in the file base.", stats["files"], lbl)
    _emit_areas(exp, bbs, "files", stats["files_by_area"])
    exp.add(
        "enigma_file_bytes",
        "gauge",
        "Bytes held in the file base.",
        stats["file_bytes"],
        lbl,
    )
    exp.add(
        "enigma_file_downloads_total",
        "counter",
        "Downloads across every file currently in the base. Falls if a file is "
        "removed, which reads as a counter reset.",
        stats["downloads"],
        lbl,
    )

    # Freshness. Absent rather than zero when the board has never been called:
    # a zero here would plot as 1970 and swamp the axis.
    for metric, key, help_text in (
        ("enigma_last_login_timestamp_seconds", "last_login", "Unix time of the most recent login."),
        ("enigma_last_message_timestamp_seconds", "last_message", "Unix time of the most recent message."),
        ("enigma_last_upload_timestamp_seconds", "last_upload", "Unix time of the most recent upload."),
    ):
        if stats.get(key) is not None:
            exp.add(metric, "gauge", help_text, "%.3f" % stats[key], lbl)

    for name, size in sorted(stats["db_sizes"].items()):
        exp.add(
            "enigma_db_size_bytes",
            "gauge",
            "Size on disk of each SQLite database, including its write-ahead log.",
            size,
            {"bbs": bbs, "db": name},
        )

    exp.add(
        "enigma_board_info",
        "gauge",
        "Board identity. The name is a label here rather than on every series "
        "so renaming the board does not orphan its history.",
        1,
        {"bbs": bbs, "board": board},
    )


def collect(root: str, now: int | None = None) -> str:
    """Render the exposition text for one scrape across every instance."""
    now = int(now if now is not None else time.time())
    started = time.time()
    exp = Exposition()

    instances = discover(root)
    for bbs, path in instances:
        board = board_name(path, bbs)
        try:
            stats = read_instance(path, now)
        except Exception as err:
            # A stopped board, a half-written database, a missing file. Report
            # it down and keep going: one sick instance must not blank the
            # other boards' dashboards.
            print("scrape of %s failed: %s" % (bbs, err), file=sys.stderr, flush=True)
            exp.add(
                "enigma_up",
                "gauge",
                "Whether this board's databases could be read.",
                0,
                {"bbs": bbs},
            )
            exp.add(
                "enigma_board_info",
                "gauge",
                "Board identity. The name is a label here rather than on every "
                "series so renaming the board does not orphan its history.",
                1,
                {"bbs": bbs, "board": board},
            )
            continue
        _emit_instance(exp, bbs, board, stats)

    exp.add(
        "enigma_instances_discovered",
        "gauge",
        "Instance directories found under the exporter's root. Zero means the "
        "data directory is not mounted, which is not the same as every board "
        "being down.",
        len(instances),
    )
    exp.add(
        "enigma_scrape_duration_seconds",
        "gauge",
        "Time taken to read every instance for this scrape.",
        "%.6f" % (time.time() - started),
    )
    return exp.render()


class Handler(BaseHTTPRequestHandler):
    root = DEFAULT_ROOT

    def do_GET(self):
        if self.path.split("?")[0] not in ("/metrics", "/"):
            self.send_error(404)
            return
        body = collect(self.root).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format, *args):  # noqa: A002 - signature is the base class's
        pass  # a scrape every 15s is not news


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--root", default=os.environ.get("BBS_ROOT", DEFAULT_ROOT))
    ap.add_argument("--port", type=int, default=int(os.environ.get("PORT", DEFAULT_PORT)))
    args = ap.parse_args()

    Handler.root = args.root
    found = ", ".join(name for name, _ in discover(args.root)) or "none yet"
    print(
        "enigma exporter on :%d reading %s (instances: %s)"
        % (args.port, args.root, found),
        flush=True,
    )
    HTTPServer(("", args.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
