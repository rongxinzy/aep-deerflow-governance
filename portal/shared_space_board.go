package main

var sharedTaskStatuses = []string{"backlog", "todo", "in_progress", "in_review", "blocked", "done", "cancelled"}

func sharedTaskStatus(status string) bool {
	for _, candidate := range sharedTaskStatuses {
		if status == candidate {
			return true
		}
	}
	return false
}
