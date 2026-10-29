#!/usr/bin/env python3
"""Build a speaker-aware FTS5 index for transcript search.

The ccrider database is the source. This index is the search surface.
Tool text is left out. Speaker and kind are columns in the index, so a
ranked query can filter them without scanning every match.
"""

import sqlite3
import sys
import time
import unicodedata

SRC = "/home/jon/.config/ccrider/sessions.db"
BATCH = 800
BODY = 800

HANDOFF = (
    "FIRSTMATE_OP:",
    "Beads enforcement",
    "Workspace boundary",
    "PREVIOUS AI RESPONSE",
    "BEADS NOTIFY",
    "Firstmate instruction",
    ": Firstmate instruction",
    "Analyze this Claude Code",
    "reply with exactly:",
    "Reply to me at:",
    "<recommended_plugins>",
    "<task-notification>",
    "<!-- backpass",
    "You are a crewmate",
    "[fm-from-firstmate]",
    "SENTIMENT:",
)


def visible(text):
    s = text.lstrip()
    while s and unicodedata.category(s[0]) in ("Cf", "Cc"):
        s = s[1:].lstrip()
    return s


def kind_of(sender, text):
    s = visible(text)
    if sender == "assistant":
        return "assistant", s[:BODY]
    key = "CURRENT USER MESSAGE:"
    i = s.rfind(key)
    if i >= 0:
        tail = s[i + len(key):].lstrip(":\n ")
        if tail and not any(tail.startswith(p) for p in HANDOFF):
            return "typed", tail[:BODY]
    if any(s.startswith(p) for p in HANDOFF):
        return "handoff", s[:BODY]
    return "typed", s[:BODY]


def main():
    dest = sys.argv[1]
    src = sqlite3.connect(f"file:{SRC}?mode=ro", uri=True)
    dst = sqlite3.connect(dest)
    dst.execute("PRAGMA journal_mode=WAL")
    dst.execute("PRAGMA synchronous=NORMAL")
    dst.execute("PRAGMA temp_store=MEMORY")
    dst.execute(
        """CREATE VIRTUAL TABLE IF NOT EXISTS tx USING fts5(
            body, sender, kind, provider,
            session_id UNINDEXED, msg_id UNINDEXED, cwd UNINDEXED,
            summary UNINDEXED, ts UNINDEXED,
            tokenize='unicode61 remove_diacritics 2')"""
    )
    dst.execute("CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT)")
    row = dst.execute("SELECT v FROM meta WHERE k='last_id'").fetchone()
    last = int(row[0]) if row else 0
    inserted = 0
    t0 = time.time()
    while True:
        rows = src.execute(
            """SELECT m.id, m.sender, ifnull(m.text_content,''), ifnull(m.timestamp,''),
                      s.session_id, ifnull(s.provider,''), ifnull(s.cwd,''), ifnull(s.summary,'')
               FROM messages m JOIN sessions s ON s.id = m.session_id
               WHERE m.sender IN ('human','assistant') AND m.id > ?
               ORDER BY m.id LIMIT ?""",
            (last, BATCH),
        ).fetchall()
        if not rows:
            break
        batch = []
        for mid, sender, text, ts, sid, provider, cwd, summary in rows:
            last = mid
            if not text.strip():
                continue
            kind, body = kind_of(sender, text)
            if not body.strip():
                continue
            who = "assistant" if sender == "assistant" else "human"
            batch.append((body, who, kind, provider, sid, str(mid), cwd, summary[:180], ts))
        if batch:
            dst.executemany(
                "INSERT INTO tx(body, sender, kind, provider, session_id, msg_id, cwd, summary, ts) VALUES (?,?,?,?,?,?,?,?,?)",
                batch,
            )
            inserted += len(batch)
        dst.execute("INSERT INTO meta(k,v) VALUES('last_id',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v", (str(last),))
        dst.commit()
        if inserted and inserted % 8000 < BATCH:
            print(f"indexed {inserted} last_id {last} {int(time.time()-t0)}s", flush=True)
    print(f"done indexed {inserted} last_id {last} {int(time.time()-t0)}s", flush=True)


if __name__ == "__main__":
    main()
