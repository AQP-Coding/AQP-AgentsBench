package bench

import "fmt"

func ScoreTests(tests, outcomes []Record) (Record, error) {
	allowed := map[string]bool{}
	for _, t := range tests {
		allowed[text(t["id"])] = true
	}
	index := map[string]Record{}
	for _, o := range outcomes {
		id := text(o["id"])
		if index[id] != nil || !allowed[id] {
			return nil, fmt.Errorf("Duplicate or unrecognized evaluation outcome")
		}
		index[id] = o
	}
	results := []Record{}
	maximum, earned := 0.0, 0.0
	passed := 0
	for _, test := range tests {
		if err := checkNumber(test["score"], "unit-test score", false, false); err != nil {
			return nil, fmt.Errorf("Invalid unit-test score")
		}
		score := number(test["score"])
		outcome := cloneRecord(index[text(test["id"])])
		if outcome == nil {
			outcome = Record{"id": test["id"], "status": "not_run"}
		}
		outcome["max_score"], outcome["earned_score"] = score, 0.0
		maximum += score
		if outcome["status"] == "passed" {
			earned += score
			outcome["earned_score"] = score
			passed++
		}
		results = append(results, outcome)
	}
	r := Record{"tests": results, "earned_score": earned, "max_score": maximum, "score_percent": nil, "passed_percent": nil}
	if maximum != 0 {
		r["score_percent"] = 100 * earned / maximum
	}
	if len(results) > 0 {
		r["passed_percent"] = 100 * float64(passed) / float64(len(results))
	}
	return r, nil
}
func Aggregate(runs []Record) Record {
	count := 0
	sum := 0.0
	for _, r := range runs {
		status := text(r["status"])
		eval := object(r["evaluation"])
		if (status == "completed" || status == "agent_failed" || status == "incomplete") && eval["score_percent"] != nil {
			count++
			sum += number(eval["score_percent"])
		}
	}
	result := Record{"eligible_runs": count, "excluded_runs": len(runs) - count, "mean_score_percent": nil}
	if count > 0 {
		result["mean_score_percent"] = sum / float64(count)
	}
	return result
}
