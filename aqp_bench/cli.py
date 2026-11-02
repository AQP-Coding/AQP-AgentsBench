"""CLI-only run controls and human replies."""

import argparse
import json
import os
import threading
import sys
from pathlib import Path

from .manifest import expand_runs, load_campaign
from .runner import coerce_answer, run_campaign
from .scoring import aggregate
from .store import Store
from .workspace import preflight_repository


def _parser():
    parser = argparse.ArgumentParser(prog="aqp-bench", description="Local coding-agent benchmarks; Linux x86_64")
    parser.add_argument("--artifacts", default=".aqp-bench", help="Local persistent artifact directory")
    commands = parser.add_subparsers(dest="command", required=True)
    validate = commands.add_parser("validate", help="Validate a campaign without model calls or Docker")
    validate.add_argument("manifest", type=Path)
    run = commands.add_parser("run", help="Run a campaign; requires prepared Docker images and an explicit API key")
    run.add_argument("manifest", type=Path)
    run.add_argument("--api-key-env", default="OPENROUTER_API_KEY", help="Explicit environment variable containing the provider key")
    commands.add_parser("demo", help="Offline simulation: mock provider and trusted local execution, no Docker or paid calls")
    commands.add_parser("list", help="List retained run results")
    show = commands.add_parser("show", help="Print a retained run result")
    show.add_argument("run_id")
    stop = commands.add_parser("stop", help="Request agent stop; current workspace will still be evaluated")
    stop.add_argument("run_id")
    questions = commands.add_parser("questions", help="Show pending human requests and complete choice metadata")
    questions.add_argument("--run-id")
    answer = commands.add_parser("answer", help="Submit one human reply; structured question replies use JSON")
    answer.add_argument("run_id")
    answer.add_argument("request_id")
    answer.add_argument("reply", help="Text, yes/no for confirmation, JSON string[][] for OpenCode questions, or null to cancel")
    rerun = commands.add_parser("rerun", help="Explicitly rerun a retained definition as a new linked run")
    rerun.add_argument("run_id")
    rerun.add_argument("--api-key-env", default="OPENROUTER_API_KEY")
    dashboard = commands.add_parser("dashboard", help="Serve loopback-only live monitoring; no mutation controls")
    dashboard.add_argument("--port", type=int, default=8765)
    return parser


def _key(args):
    value = os.environ.get(args.api_key_env)
    if not value:
        raise ValueError(f"Set the explicitly selected API-key environment variable {args.api_key_env}")
    return value


def main(argv=None):
    args = _parser().parse_args(argv)
    try:
        if args.command == "validate":
            campaign = load_campaign(args.manifest)
            warnings = []
            for task in campaign["tasks"]:
                if task.get("repository"):
                    _, messages = preflight_repository(task["repository"])
                    warnings.extend(messages)
            print(json.dumps({"valid": True, "runs": len(expand_runs(campaign)), "warnings": warnings}))
            return 0
        if args.command == "run":
            campaign = load_campaign(args.manifest)
            key = _key(args)
            results = run_campaign(campaign, Store(args.artifacts), api_key=key,
                                   notify=lambda message: print(message, file=sys.stderr, flush=True))
            print(json.dumps({"runs": results, "aggregate": aggregate(results)}))
            return 0 if all(r["status"] == "completed" for r in results) else 1
        if args.command == "demo":
            from .demo import run_demo
            result = run_demo(Store(args.artifacts))
            print(json.dumps(result))
            return 0 if result["status"] == "completed" and result["evaluation"]["score_percent"] == 100 else 1
        if args.command in ("show", "stop", "rerun", "answer"):
            if not (Path(args.artifacts) / "bench.sqlite3").is_file():
                raise ValueError("No retained benchmark store at the selected artifact directory")
        store = Store(args.artifacts)
        if args.command == "list":
            print(json.dumps(store.list()))
        elif args.command == "show":
            print(json.dumps(store.get(args.run_id)))
        elif args.command == "stop":
            result = store.get(args.run_id)
            if result["status"] not in ("preparing", "running"):
                raise ValueError("Run is not currently executing an agent")
            store.stop(args.run_id)
            print(json.dumps({"stop_requested": args.run_id}))
        elif args.command == "questions":
            print(json.dumps([q for q in store.questions(args.run_id)
                              if store.get(q["run_id"])["status"] == "running"]))
        elif args.command == "answer":
            requests = [q for q in store.questions(args.run_id) if q["id"] == args.request_id]
            if not requests:
                raise ValueError("Unknown or already answered request")
            if store.get(args.run_id)["status"] != "running":
                raise ValueError("Run is no longer waiting for agent replies")
            coerce_answer(args.reply, requests[0]["metadata"])
            if not store.answer(args.run_id, args.request_id, args.reply):
                raise ValueError("Request was already answered")
            print(json.dumps({"answered": args.request_id}))
        elif args.command == "rerun":
            key = _key(args)
            old = store.get(args.run_id)
            if old.get("simulation"):
                raise ValueError("Use demo for offline simulation; rerun is for live definitions")
            definition = json.loads((store.run_dir(args.run_id) / "definition.json").read_text())
            campaign = {"version": 1, "tasks": [definition["task"]], "agents": [definition["agent"]],
                        "models": [definition["model"]], "harnesses": [definition["harness"]],
                        "trials": 1, "concurrency": 1, "limits": definition["limits"]}
            results = run_campaign(campaign, store, api_key=key,
                                   notify=lambda message: print(message, file=sys.stderr, flush=True))
            for result in results:
                result["rerun_of"] = args.run_id
                store.save(result)
            print(json.dumps({"runs": results, "aggregate": aggregate(results)}))
            return 0 if all(r["status"] == "completed" for r in results) else 1
        elif args.command == "dashboard":
            from .dashboard import Dashboard
            with Dashboard(store, port=args.port) as dashboard:
                print("Live monitoring: " + dashboard.url, flush=True)
                threading.Event().wait()
        return 0
    except KeyboardInterrupt:
        return 130
    except (ValueError, KeyError, OSError) as error:
        print(f"Error: {error}", file=sys.stderr)
        return 2
