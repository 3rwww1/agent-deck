package session

// LiveFirstOrder returns the indices of instances with every stored error or
// stopped row after the others, keeping input order inside each group. Status
// passes walk it so live sessions read the tmux caches before they expire.
func LiveFirstOrder(instances []*Instance) []int {
	order := make([]int, 0, len(instances))
	var dead []int
	for i, inst := range instances {
		if inst != nil {
			if s := inst.GetStatusThreadSafe(); s == StatusError || s == StatusStopped {
				dead = append(dead, i)
				continue
			}
		}
		order = append(order, i)
	}
	return append(order, dead...)
}
