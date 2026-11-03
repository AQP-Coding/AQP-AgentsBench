"""Author-assigned unit-test scores, independent from run completion status."""

import math


def score_tests(tests, outcomes):
    indexed = {item["id"]: item for item in outcomes}
    if len(indexed) != len(outcomes) or set(indexed) - {t["id"] for t in tests}:
        raise ValueError("Duplicate or unrecognized evaluation outcome")
    results = []
    for test in tests:
        maximum = test["score"]
        if isinstance(maximum, bool) or not math.isfinite(maximum) or maximum < 0:
            raise ValueError("Invalid unit-test score")
        outcome = indexed.get(test["id"], {"id": test["id"], "status": "not_run"})
        results.append({**outcome, "max_score": maximum,
                        "earned_score": maximum if outcome["status"] == "passed" else 0})
    maximum = sum(r["max_score"] for r in results)
    earned = sum(r["earned_score"] for r in results)
    return {"tests": results, "earned_score": earned, "max_score": maximum,
            "score_percent": 100 * earned / maximum if maximum else None,
            "passed_percent": 100 * sum(r["status"] == "passed" for r in results) / len(results) if results else None}


def aggregate(runs):
    eligible = [r for r in runs if r.get("status") in ("completed", "agent_failed", "incomplete")
                and r.get("evaluation", {}).get("score_percent") is not None]
    return {"eligible_runs": len(eligible), "excluded_runs": len(runs) - len(eligible),
            "mean_score_percent": sum(r["evaluation"]["score_percent"] for r in eligible) / len(eligible)
            if eligible else None}
