"""Local persistence with atomic snapshots and cross-process CLI controls."""

import json
import re
import sqlite3
import threading
import time
import uuid
from contextlib import contextmanager
from pathlib import Path


class Store:
    def __init__(self, root):
        self.root = Path(root).absolute()
        self.root.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.lock = threading.RLock()
        with self._connection() as db:
            db.execute("PRAGMA journal_mode=WAL")
            db.executescript("""
                CREATE TABLE IF NOT EXISTS runs (id TEXT PRIMARY KEY, result TEXT NOT NULL);
                CREATE TABLE IF NOT EXISTS events (seq INTEGER PRIMARY KEY, run_id TEXT, event TEXT);
                CREATE TABLE IF NOT EXISTS questions (id TEXT PRIMARY KEY, run_id TEXT, question TEXT,
                    metadata TEXT, answer TEXT, created REAL);
            """)

    @contextmanager
    def _connection(self):
        db = sqlite3.connect(self.root / "bench.sqlite3", timeout=10)
        try:
            with db:
                yield db
        finally:
            db.close()

    def run_dir(self, identifier):
        if not isinstance(identifier, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", identifier):
            raise ValueError("Invalid run identifier")
        path = self.root / "runs" / identifier
        if path.is_symlink() or (self.root / "runs").is_symlink():
            raise ValueError("Artifact directories may not be symlinks")
        path.mkdir(parents=True, exist_ok=True, mode=0o700)
        return path

    def save(self, result):
        encoded = json.dumps(result, allow_nan=False)
        with self.lock:
            path = self.run_dir(result["id"]) / "result.json"
            temp = path.with_suffix(".tmp")
            temp.write_text(encoded + "\n")
            temp.replace(path)
            with self._connection() as db:
                db.execute("INSERT INTO runs VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET result=excluded.result",
                           (result["id"], encoded))

    def get(self, identifier):
        with self._connection() as db:
            row = db.execute("SELECT result FROM runs WHERE id=?", (identifier,)).fetchone()
        if row is None:
            raise KeyError(identifier)
        return json.loads(row[0])

    def list(self):
        with self._connection() as db:
            return [json.loads(row[0]) for row in db.execute("SELECT result FROM runs ORDER BY rowid DESC")]

    def event(self, identifier, event):
        encoded = json.dumps({"timestamp": time.time(), **event}, allow_nan=False)
        with self.lock, self._connection() as db:
            db.execute("INSERT INTO events(run_id,event) VALUES (?,?)", (identifier, encoded))
            with (self.run_dir(identifier) / "transcript.jsonl").open("a") as stream:
                stream.write(encoded + "\n")

    def events(self, identifier, limit=500):
        with self._connection() as db:
            rows = db.execute("SELECT event FROM events WHERE run_id=? ORDER BY seq DESC LIMIT ?",
                              (identifier, limit)).fetchall()
        return [json.loads(row[0]) for row in reversed(rows)]

    def question(self, identifier, question, metadata):
        request_id = uuid.uuid4().hex
        with self._connection() as db:
            db.execute("INSERT INTO questions VALUES (?,?,?,?,NULL,?)",
                       (request_id, identifier, question, json.dumps(metadata), time.time()))
        return request_id

    def questions(self, identifier=None):
        with self._connection() as db:
            rows = db.execute("SELECT id,run_id,question,metadata FROM questions WHERE answer IS NULL"
                              + (" AND run_id=?" if identifier else ""),
                              (identifier,) if identifier else ()).fetchall()
        return [{"id": r[0], "run_id": r[1], "question": r[2], "metadata": json.loads(r[3])} for r in rows]

    def answer(self, identifier, request_id, text):
        if not isinstance(text, str) or not text.strip():
            raise ValueError("Answer must not be empty")
        with self._connection() as db:
            cursor = db.execute("UPDATE questions SET answer=? WHERE run_id=? AND id=? AND answer IS NULL",
                                (text, identifier, request_id))
        return cursor.rowcount == 1

    def response(self, identifier, request_id):
        with self._connection() as db:
            row = db.execute("SELECT answer FROM questions WHERE run_id=? AND id=?", (identifier, request_id)).fetchone()
        return row[0] if row else None

    def interventions(self, identifier):
        with self._connection() as db:
            return db.execute("SELECT COUNT(*) FROM questions WHERE run_id=? AND answer IS NOT NULL",
                              (identifier,)).fetchone()[0]

    def stop(self, identifier):
        self.get(identifier)
        (self.run_dir(identifier) / "stop-requested").touch()

    def stop_requested(self, identifier):
        return (self.run_dir(identifier) / "stop-requested").exists()
