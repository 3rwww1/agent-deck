package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSyncTmuxWorkDir_FollowsMovedProjectPath(t *testing.T) {
	inst := NewInstance("moved", t.TempDir())
	newDir := t.TempDir()
	inst.ProjectPath = newDir

	inst.syncTmuxWorkDir()

	assert.Equal(t, newDir, inst.GetTmuxSession().WorkDir)
	assert.False(t, inst.GetTmuxSession().WorkDirIsPlaceholder)
}
