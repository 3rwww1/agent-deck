package main

import (
	"fmt"
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// checkOpenCodeSessionConflict refuses a held id: the restart sweep would kill the other holder.
func checkOpenCodeSessionConflict(instances []*session.Instance, inst *session.Instance, value string) (string, string) {
	id := strings.TrimSpace(value)
	if id == "" {
		return "", ""
	}
	for _, other := range instances {
		if other == nil || other.ID == inst.ID || other.OpenCodeSessionID != id {
			continue
		}
		return fmt.Sprintf("opencode session %q is already bound to session %q (id: %s); clear it there first",
			id, other.Title, other.ID), ErrCodeAlreadyExists
	}
	return "", ""
}
