"""Union-of-active-time accounting and nonduplicating request metrics."""

import threading
import time


class Timeline:
    def __init__(self, clock=time.monotonic):
        self.clock = clock
        self.last = clock()
        self.agents = {}
        self.active = 0.0
        self.wait = 0.0
        self.lock = threading.RLock()

    def _tick(self, now):
        if now < self.last:
            raise ValueError("Timeline must be monotonic")
        delta = now - self.last
        active = [a for a in self.agents.values() if a["state"] == "active"]
        waiting = [a for a in self.agents.values() if a["state"] == "waiting"]
        if active:
            self.active += delta
        elif waiting:
            self.wait += delta
        for agent in active:
            agent["agent_seconds"] += delta
        for agent in waiting:
            agent["human_wait_seconds"] += delta
        self.last = now

    def event(self, event, now=None):
        kind = event.get("type")
        if kind not in ("agent_start", "agent_wait", "agent_resume", "agent_end"):
            return
        with self.lock:
            self._tick(self.clock() if now is None else now)
            identifier = event.get("agent_id", "main")
            if identifier not in self.agents:
                self.agents[identifier] = {"parent_id": event.get("parent_id"), "state": "ended",
                                           "agent_seconds": 0.0, "human_wait_seconds": 0.0}
            self.agents[identifier]["state"] = {
                "agent_start": "active", "agent_resume": "active",
                "agent_wait": "waiting", "agent_end": "ended"}[kind]

    def snapshot(self, now=None):
        with self.lock:
            self._tick(self.clock() if now is None else now)
            agents = {key: dict(value) for key, value in self.agents.items()}
            return {"agent_seconds": self.active, "human_wait_seconds": self.wait,
                    "cumulative_agent_seconds": sum(a["agent_seconds"] for a in agents.values()),
                    "agents": agents}

    def finish(self):
        with self.lock:
            self._tick(self.clock())
            for agent in self.agents.values():
                agent["state"] = "ended"


def summarize_attempts(attempts):
    unique = {a["attempt_id"]: a for a in attempts}
    values = list(unique.values())

    def total(rows):
        result = {"request_count": len(rows), "accounting_complete": True}
        for field in ("input_tokens", "output_tokens", "cost_usd"):
            known = [r[field] for r in rows if r.get(field) is not None]
            complete = len(known) == len(rows)
            result[field] = sum(known) if complete else None
            result["known_" + field] = sum(known)
            result["accounting_complete"] &= complete
        result["network_failures"] = sum(r.get("error_kind") == "network" for r in rows)
        return result

    result = total(values)
    identities = {a.get("agent_id", "unattributed") for a in values}
    result["agents"] = {identity: total([a for a in values if a.get("agent_id", "unattributed") == identity])
                        for identity in identities}
    result["attribution_complete"] = "unattributed" not in identities
    return result
