package session

// LiveFirstOrder lists indices with stored error and stopped rows last, so live rows hit warm tmux caches.
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
