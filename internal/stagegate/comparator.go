package stagegate

import "math"

// comparator helpers shared by the threshold-style leaf gates
// (databricks_query_threshold, datadog_metric_threshold). Centralised here
// so a future third threshold gate doesn't trigger a copy-paste fork.
//
// The gate config carries a string operator from the closed set
// {lt, gt, eq, lte, gte}. validComparator is the planner-side guardrail;
// compareValue is the runtime evaluation. Equality uses an absolute
// tolerance of 1e-9 to avoid float-noise false-negatives — values produced
// by SQL aggregates / Datadog rollups frequently differ from a hand-typed
// threshold by a single ULP even when they're "equal" by every operator
// expectation.

const comparatorEqualityTolerance = 1e-9

// validComparator returns true if op is one of the supported threshold
// operators. Used by gates to fail-fast on planner-side typos before any
// network call.
func validComparator(op string) bool {
	switch op {
	case "lt", "gt", "eq", "lte", "gte":
		return true
	}
	return false
}

// compareValue evaluates "value <op> threshold". Caller is expected to
// have already passed op through validComparator; an unknown op returns
// false (defensive — should never be hit at runtime).
//
// `eq` compares with an absolute tolerance of comparatorEqualityTolerance
// so that "scalar equality" semantics survive float-noise.
func compareValue(op string, value, threshold float64) bool {
	switch op {
	case "lt":
		return value < threshold
	case "gt":
		return value > threshold
	case "lte":
		return value <= threshold
	case "gte":
		return value >= threshold
	case "eq":
		return math.Abs(value-threshold) <= comparatorEqualityTolerance
	}
	return false
}
