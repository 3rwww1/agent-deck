package tmux

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRespawnPane_UsesCurrentWorkDir(t *testing.T) {
	skipIfNoTmuxBinary(t)

	oldDir := t.TempDir()
	newDir := t.TempDir()
	s := NewSession("respawn-workdir", oldDir)
	require.NoError(t, s.Start("sleep 300"))
	t.Cleanup(func() { _ = s.Kill() })
	requirePaneSettledIn(t, s, oldDir)

	s.WorkDir = newDir
	require.NoError(t, s.RespawnPane("sleep 300"))
	requirePaneSettledIn(t, s, newDir)
}

func TestRespawnPane_RefusesDeletedWorkDir(t *testing.T) {
	skipIfNoTmuxBinary(t)

	s := NewSession("respawn-missing-workdir", t.TempDir())
	require.NoError(t, s.Start("sleep 300"))
	t.Cleanup(func() { _ = s.Kill() })

	missing := filepath.Join(t.TempDir(), "gone")
	require.NoError(t, os.Mkdir(missing, 0o755))
	require.NoError(t, os.RemoveAll(missing))
	s.WorkDir = missing

	err := s.RespawnPane("sleep 300")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrWorkDirUnavailable))
}
