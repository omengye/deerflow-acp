package eino

// Eino treats zero as its own default of 20 iterations. Keep zero in the
// harness configuration and immutable execution policy, but translate it at
// the SDK boundary to the largest representable counter. No operational quota
// is imposed; cancellation, execution deadlines and explicit budgets still
// stop the agent. This avoids inheriting another hidden SDK default.
func nativeMaxIterations(limit int) int {
	if limit == 0 {
		return int(^uint(0) >> 1)
	}
	return limit
}
