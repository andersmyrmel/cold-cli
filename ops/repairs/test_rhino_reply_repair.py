"""Rehearse the exact repair against a tiny, disposable local Docker database.

Run: python3 ops/repairs/test_rhino_reply_repair.py
Owns and drops only its UUID-named coldcli_rhino_repair_* database.
"""

import json
from pathlib import Path
import re
import subprocess
import unittest
import uuid


CONTAINER = "store-inspector-postgres-1"
REPAIR = Path(__file__).with_name("2026-10-26-rhino-human-replies.sql").read_text()


class RhinoRepairTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.database = "coldcli_rhino_repair_" + uuid.uuid4().hex[:12]
        cls.psql("CREATE DATABASE " + cls.database, database="postgres")
        try:
            cls.psql("""
                CREATE TABLE campaigns (id bigint PRIMARY KEY, workspace_id text, name text, status text);
                CREATE TABLE leads (id bigint PRIMARY KEY, email text);
                CREATE TABLE campaign_leads (campaign_id bigint, lead_id bigint, status text);
                CREATE TABLE scheduled_sends (campaign_id bigint, status text);
                CREATE TABLE events (id bigint PRIMARY KEY,campaign_id bigint,lead_id bigint,account_id bigint,
                    type text,message_id text,thread_id text,timestamp timestamptz,metadata text);
                CREATE TABLE email_messages (id bigint PRIMARY KEY,campaign_id bigint,lead_id bigint,account_id bigint,
                    direction text,type text,message_id text,thread_id text,occurred_at timestamptz,raw_headers text);
            """)
        except BaseException:
            cls.tearDownClass()
            raise

    @classmethod
    def tearDownClass(cls):
        cls.psql("DROP DATABASE " + cls.database, database="postgres")

    @classmethod
    def psql(cls, sql, *, database=None, apply=False, check=True):
        # POSTGRES_USER is expanded inside the known local Docker container.
        result = subprocess.run(
            ["docker", "exec", "-i", CONTAINER, "sh", "-c",
             'exec psql -X -qAt -v ON_ERROR_STOP=1 -v apply="$2" -U "$POSTGRES_USER" -d "$1"',
             "repair-test", database or cls.database, str(apply).lower()],
            input=sql, text=True, capture_output=True, timeout=30,
        )
        if check and result.returncode:
            raise AssertionError(result.stderr)
        return result

    def setUp(self):
        self.psql("""
            TRUNCATE campaigns,leads,campaign_leads,scheduled_sends,events,email_messages;
            INSERT INTO campaigns VALUES (226,'productlair','publisher-partnerships-2026-10-12','completed');
            INSERT INTO leads VALUES (5441,'hello@rhinorank.io');
            INSERT INTO campaign_leads VALUES (226,5441,'active');
            INSERT INTO scheduled_sends SELECT 226,'sent' FROM generate_series(1,30);
            INSERT INTO events VALUES
                (2416,226,5441,3,'auto_reply','1a13a97bbc8e7a0b','1a12f2e2dcf5d5d0','2026-10-14T13:26:23Z','{"retained":"yes"}'),
                (2707,226,5441,3,'auto_reply','1a178a007db2e99b','1a12f2e2dcf5d5d0','2026-10-26T14:31:53Z','{}');
            INSERT INTO email_messages
                SELECT CASE id WHEN 2416 THEN 2066 ELSE 2357 END,campaign_id,lead_id,account_id,
                    'inbound',type,message_id,thread_id,timestamp,
                    '{"X-Mailer":"Zendesk Mailer","Auto-Submitted":"auto-generated"}' FROM events;
        """)

    def state(self):
        return self.psql("""SELECT json_build_object(
            'events',(SELECT json_agg(e ORDER BY id) FROM events e),
            'messages',(SELECT json_agg(em ORDER BY id) FROM email_messages em),
            'leads',(SELECT json_agg(cl) FROM campaign_leads cl),
            'sends',(SELECT json_agg(s) FROM scheduled_sends s),
            'campaigns',(SELECT json_agg(c) FROM campaigns c))""").stdout

    def test_preview_rolls_back(self):
        before = self.state()
        result = self.psql(REPAIR)
        self.assertIn("replied", result.stdout)
        self.assertEqual(before, self.state())

    def test_runner_lock_blocks_the_repair(self):
        source = Path(__file__).parents[2].joinpath("internal/store.go").read_text()
        key = int(re.search(r"postgresTickLockKey int64 = (0x[0-9a-f]+)", source)[1], 16)
        lock = subprocess.Popen(
            ["docker", "exec", "-i", CONTAINER, "sh", "-c",
             'exec psql -X -qAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$1"', "repair-test", self.database],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
        )
        try:
            lock.stdin.write(f"SELECT pg_advisory_lock({key});\n\\echo LOCK_READY\n")
            lock.stdin.flush()
            while lock.stdout.readline().strip() != "LOCK_READY":
                if lock.poll() is not None:
                    self.fail("local lock holder exited unexpectedly")
            before = self.state()
            result = self.psql(REPAIR, apply=True, check=False)
            self.assertNotEqual(0, result.returncode)
            self.assertIn("tick lock is busy", result.stderr)
            self.assertEqual(before, self.state())
        finally:
            lock.communicate("\\q\n", timeout=5)

    def test_apply_preserves_history_and_is_idempotent(self):
        before = json.loads(self.state())
        self.psql(REPAIR, apply=True)
        after = json.loads(self.state())
        self.assertEqual(["reply", "reply"], [row["type"] for row in after["events"]])
        self.assertEqual(["reply", "reply"], [row["type"] for row in after["messages"]])
        self.assertEqual("replied", after["leads"][0]["status"])
        self.assertEqual(before["sends"], after["sends"])
        self.assertEqual(before["campaigns"], after["campaigns"])
        self.assertEqual([row["timestamp"] for row in before["events"]], [row["timestamp"] for row in after["events"]])
        metadata = json.loads(after["events"][0]["metadata"])
        self.assertEqual("yes", metadata["retained"])
        self.assertEqual("auto_reply", metadata["classification_corrections"][0]["previous_type"])
        self.psql(REPAIR, apply=True)
        self.assertEqual(after, json.loads(self.state()))

    def test_mismatch_aborts_everything(self):
        for mutation in [
            "UPDATE campaigns SET workspace_id='wrong'",
            "UPDATE email_messages SET raw_headers='{}' WHERE id=2066",
            "UPDATE events SET timestamp=now() WHERE id=2416",
            "UPDATE scheduled_sends SET status='pending'",
            "UPDATE events SET type='reply' WHERE id=2416",
        ]:
            with self.subTest(mutation=mutation):
                self.setUp()
                self.psql(mutation)
                before = self.state()
                result = self.psql(REPAIR, apply=True, check=False)
                self.assertNotEqual(0, result.returncode)
                self.assertEqual(before, self.state())


if __name__ == "__main__":
    unittest.main()
