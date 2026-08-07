"""Tests for the ENiGMA BBS Prometheus exporter.

Run: python3 -m unittest discover -s monitoring/bbs-exporter

The fixtures build ENiGMA's real schema rather than a convenient subset. A
partial mock would hide exactly the coupling that matters here -- meta_value
being VARCHAR, timestamps carrying an explicit offset, aggregates returning
NULL on a board nobody has called yet.
"""
import os
import sqlite3
import tempfile
import unittest

from exporter import AREA_CAP, board_name, collect, discover, parse_timestamp

NOW = 1_800_000_000  # 2027-01-15T08:00:00Z, fixed so the 24h window is stable
RECENT = "2027-01-15T04:00:00.000+00:00"  # inside NOW-24h
OLD = "2026-01-15T04:00:00.000+00:00"  # outside it

SCHEMA = {
    "system": [
        """CREATE TABLE system_stat (
             stat_name VARCHAR PRIMARY KEY NOT NULL, stat_value VARCHAR NOT NULL)""",
        """CREATE TABLE system_event_log (
             id INTEGER PRIMARY KEY, timestamp DATETIME NOT NULL,
             log_name VARCHAR NOT NULL, log_value VARCHAR NOT NULL,
             UNIQUE(timestamp, log_name))""",
        """CREATE TABLE user_event_log (
             id INTEGER PRIMARY KEY, timestamp DATETIME NOT NULL,
             user_id INTEGER NOT NULL, session_id VARCHAR NOT NULL,
             log_name VARCHAR NOT NULL, log_value VARCHAR NOT NULL,
             UNIQUE(timestamp, user_id, session_id, log_name))""",
    ],
    "user": [
        "CREATE TABLE user (id INTEGER PRIMARY KEY, user_name VARCHAR NOT NULL UNIQUE)",
        """CREATE TABLE user_property (
             user_id INTEGER NOT NULL, prop_name VARCHAR NOT NULL,
             prop_value VARCHAR, UNIQUE(user_id, prop_name))""",
    ],
    "message": [
        """CREATE TABLE message (
             message_id INTEGER PRIMARY KEY, area_tag VARCHAR NOT NULL,
             message_uuid VARCHAR(36) NOT NULL, to_user_name VARCHAR NOT NULL,
             from_user_name VARCHAR NOT NULL, subject, message,
             modified_timestamp DATETIME NOT NULL)""",
    ],
    "file": [
        """CREATE TABLE file (
             file_id INTEGER PRIMARY KEY, area_tag VARCHAR NOT NULL,
             file_sha256 VARCHAR NOT NULL, file_name, storage_tag VARCHAR NOT NULL,
             desc, desc_long, upload_timestamp DATETIME NOT NULL)""",
        """CREATE TABLE file_meta (
             file_id INTEGER NOT NULL, meta_name VARCHAR NOT NULL,
             meta_value VARCHAR NOT NULL, UNIQUE(file_id, meta_name, meta_value))""",
    ],
}


def build_instance(root, name, wal=False):
    """Create an empty but schema-complete instance directory."""
    path = os.path.join(root, name)
    os.makedirs(os.path.join(path, "db"))
    holders = []
    for db_name, statements in SCHEMA.items():
        db = sqlite3.connect(os.path.join(path, "db", "%s.sqlite3" % db_name))
        if wal:
            db.execute("PRAGMA journal_mode=WAL")
        for sql in statements:
            db.execute(sql)
        db.commit()
        if wal:
            holders.append(db)  # keep open so the WAL is not checkpointed away
        else:
            db.close()
    return path, holders


def populate(path):
    """Fill an instance with the shapes the live boards actually hold."""
    db = sqlite3.connect(os.path.join(path, "db", "system.sqlite3"))
    db.executemany(
        "INSERT INTO system_stat VALUES (?, ?)",
        [("login_count", "48"), ("ul_total_count", "96"), ("ul_total_bytes", "888947618")],
    )
    db.executemany(
        "INSERT INTO system_event_log (timestamp, log_name, log_value) VALUES (?,?,?)",
        [
            (RECENT, "user_login_history", '{"userId":15,"sessionId":"a"}'),
            (RECENT[:-9] + "1.000+00:00", "user_login_history", '{"userId":15,"sessionId":"b"}'),
            (RECENT[:-9] + "2.000+00:00", "user_login_history", '{"userId":7,"sessionId":"c"}'),
            (OLD, "user_login_history", '{"userId":3,"sessionId":"d"}'),
        ],
    )
    db.executemany(
        "INSERT INTO user_event_log (timestamp,user_id,session_id,log_name,log_value) "
        "VALUES (?,?,?,?,?)",
        [
            (RECENT, 1, "s1", "login", "1"),
            (RECENT[:-9] + "1.000+00:00", 1, "s1", "logoff", "1"),
            (RECENT[:-9] + "2.000+00:00", 2, "s2", "login", "1"),
        ],
    )
    db.commit()
    db.close()

    db = sqlite3.connect(os.path.join(path, "db", "user.sqlite3"))
    db.executemany("INSERT INTO user VALUES (?,?)", [(1, "alice"), (2, "bob"), (3, "carol")])
    db.executemany(
        "INSERT INTO user_property VALUES (?,?,?)",
        [
            (1, "account_status", "2"), (2, "account_status", "2"), (3, "account_status", "1"),
            (1, "minutes_online_total_count", "100"), (2, "minutes_online_total_count", "72"),
            (1, "achievement_total_points", "40"), (2, "achievement_total_points", "15"),
            (1, "last_login_timestamp", RECENT), (2, "last_login_timestamp", OLD),
        ],
    )
    db.commit()
    db.close()

    db = sqlite3.connect(os.path.join(path, "db", "message.sqlite3"))
    db.executemany(
        "INSERT INTO message (area_tag,message_uuid,to_user_name,from_user_name,"
        "subject,message,modified_timestamp) VALUES (?,?,?,?,?,?,?)",
        [("private_mail", "u%d" % i, "a", "b", "s", "m", RECENT) for i in range(14)],
    )
    db.commit()
    db.close()

    db = sqlite3.connect(os.path.join(path, "db", "file.sqlite3"))
    areas = ["apps"] * 36 + ["games"] * 14 + ["iso"] * 10 + ["zero_day"] * 25
    db.executemany(
        "INSERT INTO file (file_id,area_tag,file_sha256,file_name,storage_tag,"
        "desc,desc_long,upload_timestamp) VALUES (?,?,?,?,?,?,?,?)",
        [(i, a, "h%d" % i, "f%d" % i, "st", "d", "dl", RECENT) for i, a in enumerate(areas)],
    )
    # meta_value is VARCHAR on the real board; these must be CAST to sum.
    db.executemany(
        "INSERT INTO file_meta VALUES (?,?,?)",
        [(i, "byte_size", "1000") for i in range(len(areas))]
        + [(i, "dl_count", "2") for i in range(len(areas))],
    )
    db.commit()
    db.close()


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


class EnigmaExporterTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = self.tmp.name
        self.addCleanup(self.tmp.cleanup)

    def test_populated_board_reports_its_real_totals(self):
        path, _ = build_instance(self.root, "bbs1")
        populate(path)
        out = collect(self.root, now=NOW)

        self.assertEqual(sample(out, "enigma_up", bbs="bbs1"), "1")
        self.assertEqual(sample(out, "enigma_logins_total", bbs="bbs1"), "48")
        self.assertEqual(sample(out, "enigma_uploads_total", bbs="bbs1"), "96")
        self.assertEqual(sample(out, "enigma_upload_bytes_total", bbs="bbs1"), "888947618")
        self.assertEqual(sample(out, "enigma_users", bbs="bbs1"), "3")
        self.assertEqual(sample(out, "enigma_messages", bbs="bbs1"), "14")
        self.assertEqual(sample(out, "enigma_files", bbs="bbs1"), "85")
        self.assertEqual(sample(out, "enigma_file_bytes", bbs="bbs1"), "85000")
        self.assertEqual(sample(out, "enigma_file_downloads_total", bbs="bbs1"), "170")
        self.assertEqual(sample(out, "enigma_minutes_online_total", bbs="bbs1"), "172")
        self.assertEqual(sample(out, "enigma_achievement_points_total", bbs="bbs1"), "55")
        self.assertEqual(sample(out, "enigma_files_by_area", bbs="bbs1", area="apps"), "36")
        self.assertEqual(sample(out, "enigma_messages_by_area", bbs="bbs1", area="private_mail"), "14")
        self.assertEqual(sample(out, "enigma_users_by_status", bbs="bbs1", status="2"), "2")

    def test_24h_window_excludes_older_calls_and_counts_callers_once(self):
        path, _ = build_instance(self.root, "bbs1")
        populate(path)
        out = collect(self.root, now=NOW)

        # Three logins inside the window, one a year old and excluded.
        self.assertEqual(sample(out, "enigma_logins_last_24h", bbs="bbs1"), "3")
        # Two of those three are the same user, so two distinct callers.
        self.assertEqual(sample(out, "enigma_callers_last_24h", bbs="bbs1"), "2")
        # The trimmed history table still holds all four rows.
        self.assertEqual(sample(out, "enigma_login_history_recorded", bbs="bbs1"), "4")

    def test_trimmed_tables_are_gauges_and_system_stats_are_counters(self):
        """The bans_recorded trap: a DELETE-trimmed table read as a counter
        makes every trim look like a counter reset and rate() invents traffic."""
        path, _ = build_instance(self.root, "bbs1")
        populate(path)
        out = collect(self.root, now=NOW)

        self.assertEqual(type_of(out, "enigma_user_events_recorded"), "gauge")
        self.assertEqual(type_of(out, "enigma_login_history_recorded"), "gauge")
        self.assertEqual(type_of(out, "enigma_logins_total"), "counter")
        self.assertEqual(type_of(out, "enigma_uploads_total"), "counter")
        self.assertEqual(type_of(out, "enigma_upload_bytes_total"), "counter")
        self.assertEqual(sample(out, "enigma_user_events_recorded", bbs="bbs1", event="login"), "2")

    def test_wal_content_is_read_rather_than_the_stale_main_file(self):
        """The immutable=1 trap: a WAL database read with immutable=1 returns
        the main file and silently ignores rows still in the -wal. Measured on a
        live board as 12 users where the truth was 15."""
        path, holders = build_instance(self.root, "bbs1", wal=True)
        self.addCleanup(lambda: [h.close() for h in holders])

        db = sqlite3.connect(os.path.join(path, "db", "user.sqlite3"))
        db.execute("PRAGMA journal_mode=WAL")
        db.executemany("INSERT INTO user VALUES (?,?)", [(i, "u%d" % i) for i in range(15)])
        db.commit()
        self.addCleanup(db.close)  # stays open, so the WAL is never checkpointed

        out = collect(self.root, now=NOW)
        self.assertEqual(sample(out, "enigma_users", bbs="bbs1"), "15")

    def test_fresh_board_reports_zeros_and_omits_timestamps(self):
        """bbs2 and bbs3 are installed but never called: every aggregate is
        NULL. A zeroed timestamp would plot as 1970 and swamp the axis."""
        build_instance(self.root, "bbs2")
        out = collect(self.root, now=NOW)

        self.assertEqual(sample(out, "enigma_up", bbs="bbs2"), "1")
        self.assertEqual(sample(out, "enigma_users", bbs="bbs2"), "0")
        self.assertEqual(sample(out, "enigma_logins_total", bbs="bbs2"), "0")
        self.assertEqual(sample(out, "enigma_file_bytes", bbs="bbs2"), "0")
        self.assertEqual(sample(out, "enigma_minutes_online_total", bbs="bbs2"), "0")
        self.assertIsNone(sample(out, "enigma_last_login_timestamp_seconds", bbs="bbs2"))

    def test_one_broken_board_does_not_blank_the_others(self):
        good, _ = build_instance(self.root, "bbs1")
        populate(good)
        broken, _ = build_instance(self.root, "bbs2")
        os.remove(os.path.join(broken, "db", "user.sqlite3"))

        out = collect(self.root, now=NOW)
        self.assertEqual(sample(out, "enigma_up", bbs="bbs2"), "0")
        self.assertEqual(sample(out, "enigma_up", bbs="bbs1"), "1")
        self.assertEqual(sample(out, "enigma_users", bbs="bbs1"), "3")
        # Identity survives so the dashboard still names the board it lost.
        self.assertEqual(sample(out, "enigma_board_info", bbs="bbs2", board="bbs2"), "1")

    def test_area_cardinality_is_capped_and_the_shortfall_published(self):
        path, _ = build_instance(self.root, "bbs1")
        db = sqlite3.connect(os.path.join(path, "db", "file.sqlite3"))
        db.executemany(
            "INSERT INTO file (file_id,area_tag,file_sha256,file_name,storage_tag,"
            "desc,desc_long,upload_timestamp) VALUES (?,?,?,?,?,?,?,?)",
            [(i, "area%03d" % i, "h", "f", "st", "d", "dl", RECENT) for i in range(AREA_CAP + 7)],
        )
        db.commit()
        db.close()

        out = collect(self.root, now=NOW)
        emitted = [ln for ln in out.splitlines() if ln.startswith("enigma_files_by_area{")]
        self.assertEqual(len(emitted), AREA_CAP)
        self.assertEqual(sample(out, "enigma_areas_truncated", bbs="bbs1", kind="files"), "7")

    def test_board_name_is_read_from_hjson_in_both_quoted_and_bare_forms(self):
        path, _ = build_instance(self.root, "bbs1")
        os.makedirs(os.path.join(path, "config"))
        cfg = os.path.join(path, "config", "config.hjson")

        for written, expected in (
            ("{\n    general: {\n        boardName: Fungus Land\n    }\n}", "Fungus Land"),
            ('{\n  "boardName": "Happy Friends BBS",\n}', "Happy Friends BBS"),
        ):
            with open(cfg, "w") as fh:
                fh.write(written)
            self.assertEqual(board_name(path, "bbs1"), expected)

        os.remove(cfg)
        self.assertEqual(board_name(path, "bbs1"), "bbs1")

    def test_discovery_ignores_directories_without_a_db(self):
        build_instance(self.root, "bbs1")
        os.makedirs(os.path.join(self.root, "not-an-instance"))
        self.assertEqual([n for n, _ in discover(self.root)], ["bbs1"])

    def test_missing_root_reports_zero_instances_rather_than_crashing(self):
        """Zero discovered is a distinct signal from every board being down --
        it means the data directory was never mounted."""
        out = collect(os.path.join(self.root, "nope"), now=NOW)
        self.assertEqual(sample(out, "enigma_instances_discovered"), "0")

    def test_timestamps_parse_the_offset_form_enigma_actually_writes(self):
        self.assertEqual(parse_timestamp("2026-08-04T07:07:33.173+00:00"), 1785827253.173)
        self.assertIsNone(parse_timestamp(None))
        self.assertIsNone(parse_timestamp("not a date"))


if __name__ == "__main__":
    unittest.main()
