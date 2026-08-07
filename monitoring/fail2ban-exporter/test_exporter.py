"""Tests for the fail2ban Prometheus exporter.

Run: python3 -m unittest discover -s monitoring/fail2ban-exporter
"""
import os
import sqlite3
import tempfile
import unittest

from exporter import IP_CAP, collect

NOW = 1_800_000_000


def build_db(path, bips=(), bans=(), jails=("telix", "recidive")):
    """Build a database with fail2ban's real schema."""
    db = sqlite3.connect(path)
    db.executescript(
        """
        CREATE TABLE jails(name TEXT NOT NULL UNIQUE, enabled INTEGER NOT NULL DEFAULT 1);
        CREATE TABLE bips(ip TEXT NOT NULL, jail TEXT NOT NULL, timeofban INTEGER NOT NULL,
                          bantime INTEGER NOT NULL, bancount INTEGER NOT NULL DEFAULT 1,
                          data JSON, PRIMARY KEY(ip, jail));
        CREATE TABLE bans(jail TEXT NOT NULL, ip TEXT, timeofban INTEGER NOT NULL,
                          bantime INTEGER NOT NULL, bancount INTEGER NOT NULL DEFAULT 1, data JSON);
        """
    )
    db.executemany("INSERT INTO jails(name) VALUES (?)", [(j,) for j in jails])
    db.executemany("INSERT INTO bips(ip, jail, timeofban, bantime) VALUES (?,?,?,?)", bips)
    db.executemany("INSERT INTO bans(jail, ip, timeofban, bantime) VALUES (?,?,?,?)", bans)
    db.commit()
    db.close()


class ExporterTest(unittest.TestCase):
    def render(self, **kwargs):
        fd, path = tempfile.mkstemp(suffix=".sqlite3")
        os.close(fd)
        self.addCleanup(os.unlink, path)
        build_db(path, **kwargs)
        return collect(path, now=NOW)

    def sample(self, text, name):
        """Every exposition line for a metric, without the HELP/TYPE header."""
        return [ln for ln in text.splitlines()
                if ln.startswith(name + "{") or ln.startswith(name + " ")]

    def test_an_expired_ban_is_not_reported_as_current(self):
        # bips keeps rows after the ban lapses — fail2ban purges them lazily —
        # so a naive "SELECT * FROM bips" reports IPs that are free to connect.
        out = self.render(bips=[
            ("203.0.113.9", "telix", NOW - 100, 7200),    # live: 2h ban, 100s old
            ("198.51.100.4", "telix", NOW - 7300, 7200),  # lapsed 100s ago
        ])

        self.assertIn('telix_fail2ban_banned{jail="telix"} 1', out)
        self.assertIn('ip="203.0.113.9"', out)
        self.assertNotIn("198.51.100.4", out)

    def test_a_permanent_ban_never_expires(self):
        # fail2ban writes bantime -1 for a permanent ban; naive arithmetic
        # (timeofban + -1 > now) drops it immediately.
        out = self.render(bips=[("203.0.113.9", "telix", NOW - 999999, -1)])

        self.assertIn('telix_fail2ban_banned{jail="telix"} 1', out)
        self.assertIn('ip="203.0.113.9"', out)

    def test_the_ban_value_is_when_it_expires(self):
        out = self.render(bips=[("203.0.113.9", "telix", NOW - 100, 7200)])

        self.assertIn(
            'telix_fail2ban_banned_ip{jail="telix",ip="203.0.113.9"} %d' % (NOW - 100 + 7200),
            out,
        )

    def test_a_permanent_ban_reports_an_infinite_expiry(self):
        out = self.render(bips=[("203.0.113.9", "telix", NOW, -1)])

        self.assertIn('telix_fail2ban_banned_ip{jail="telix",ip="203.0.113.9"} +Inf', out)

    def test_a_jail_with_no_bans_still_reports_zero(self):
        # An absent series makes rate() and stat panels read "No data" rather
        # than zero, which looks like the exporter is broken.
        out = self.render(bips=[("203.0.113.9", "telix", NOW, 7200)])

        self.assertIn('telix_fail2ban_banned{jail="recidive"} 0', out)

    def test_the_ip_list_is_capped_and_says_how_many_it_dropped(self):
        # A subnet sweep must not be able to mint unbounded series in one
        # scrape. Truncation is reported, never silent.
        over = IP_CAP + 7
        out = self.render(bips=[
            ("192.0.2.%d" % i, "telix", NOW - i, 7200) for i in range(over)
        ])

        self.assertEqual(len(self.sample(out, "telix_fail2ban_banned_ip")), IP_CAP)
        # The count gauge still tells the truth about the total.
        self.assertIn('telix_fail2ban_banned{jail="telix"} %d' % over, out)
        self.assertIn("telix_fail2ban_banned_ip_truncated 7", out)

    def test_the_newest_bans_survive_truncation(self):
        out = self.render(bips=[
            ("192.0.2.%d" % i, "telix", NOW - i * 10, 7200) for i in range(IP_CAP + 5)
        ])

        self.assertIn('ip="192.0.2.0"', out)                      # newest
        self.assertNotIn('ip="192.0.2.%d"' % (IP_CAP + 4), out)   # oldest

    def test_ban_records_are_read_from_history(self):
        out = self.render(
            bans=[("telix", "203.0.113.9", NOW - 5, 7200), ("telix", "192.0.2.1", NOW - 4, 7200)],
        )

        self.assertIn('telix_fail2ban_bans_recorded{jail="telix"} 2', out)

    def test_ban_records_are_a_gauge_because_unbanning_deletes_history(self):
        # `fail2ban-client unban` removes the row from the bans table as well as
        # from bips — measured against a live fail2ban, both drop to zero. As a
        # counter, every unban would read as a counter reset and rate() would
        # invent bans that never happened.
        out = self.render(bans=[("telix", "203.0.113.9", NOW - 5, 7200)])

        self.assertIn("# TYPE telix_fail2ban_bans_recorded gauge", out)
        self.assertNotIn("telix_fail2ban_bans_total", out)

    def test_a_missing_database_reports_down_rather_than_crashing(self):
        out = collect("/nonexistent/fail2ban.sqlite3", now=NOW)

        self.assertIn("telix_fail2ban_up 0", out)

    def test_a_readable_database_reports_up(self):
        self.assertIn("telix_fail2ban_up 1", self.render())

    def test_label_values_are_escaped(self):
        # The ip column is TEXT and fail2ban is not the only thing that can put
        # bytes in it; an unescaped quote would produce unparseable exposition.
        out = self.render(bips=[('1.2.3.4"\\x', "telix", NOW, 7200)])

        self.assertIn(r'ip="1.2.3.4\"\\x"', out)


if __name__ == "__main__":
    unittest.main()
