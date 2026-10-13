package session

import (
	"strings"

	"github.com/asheshgoplani/agent-deck/internal/tmux"
)

// NewListingPass is for serial CLI listing loops only.
func NewListingPass() *StatusUpdatePass {
	return &StatusUpdatePass{pinLiveness: true}
}

// CLIStatusCandidates avoids probing each stopped session separately. A stopped
// row with no tmux session in a complete socket listing retains its stored
// status; an indeterminate listing also retains it, marked as cached.
func (p *StatusUpdatePass) CLIStatusCandidates(instances []*Instance) ([]*Instance, map[*Instance]bool) {
	refresh := make([]*Instance, 0, len(instances))
	cached := make(map[*Instance]bool)
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		if inst.Status != StatusStopped {
			refresh = append(refresh, inst)
			continue
		}
		sess := inst.GetTmuxSession()
		if sess == nil {
			cached[inst] = true
			continue
		}
		names, _ := p.listSessionNames(sess.SocketName)
		if _, exists := names[sess.Name]; !exists {
			cached[inst] = true
			continue
		}
		refresh = append(refresh, inst)
	}
	return refresh, cached
}

// sessionNames is false outside a listing pass or for an indeterminate listing.
func (p *StatusUpdatePass) sessionNames(socket string) (map[string]struct{}, bool) {
	if p == nil || !p.pinLiveness {
		return nil, false
	}
	return p.listSessionNames(socket)
}

// listSessionNames keys on the trimmed name, as tmuxArgs resolves -L.
func (p *StatusUpdatePass) listSessionNames(socket string) (map[string]struct{}, bool) {
	key := strings.TrimSpace(socket)
	p.mu.Lock()
	if names, ok := p.liveNames[key]; ok {
		p.mu.Unlock()
		return names, names != nil
	}
	if ready := p.liveLoading[key]; ready != nil {
		p.mu.Unlock()
		<-ready
		return p.listSessionNames(key)
	}
	if p.liveLoading == nil {
		p.liveLoading = make(map[string]chan struct{})
	}
	ready := make(chan struct{})
	p.liveLoading[key] = ready
	p.mu.Unlock()
	names, err := tmux.ListSessionNamesOnSocket(key)
	if err != nil {
		names = nil
	}
	p.mu.Lock()
	if p.liveNames == nil {
		p.liveNames = make(map[string]map[string]struct{})
	}
	p.liveNames[key] = names
	delete(p.liveLoading, key)
	close(ready)
	p.mu.Unlock()
	return names, names != nil
}
