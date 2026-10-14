package tmux

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tmux sends a failed command's message as the guarded lines; %error itself carries only time, number and flags.
func TestControlPipe_HandshakeErrorCarriesTmuxMessage(t *testing.T) {
	name := createTestSessionStrict(t, "handshake-error")

	_, err := NewControlPipe(name+"-missing", "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "can't find session")
}

func TestControlPipe_CommandErrorCarriesTmuxMessage(t *testing.T) {
	name := createTestSessionStrict(t, "command-error")
	pipe, err := NewControlPipe(name, "")
	require.NoError(t, err)
	defer pipe.Close()

	_, err = pipe.SendCommand("has-session -t " + name + "-missing")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "can't find session")
}

func TestControlErrorText_FallsBackToRawLine(t *testing.T) {
	assert.Equal(t, "%error 1 2 0", controlErrorText("%error 1 2 0", nil))
	assert.Equal(t, "can't find session: x", controlErrorText("%error 1 2 0", []string{"can't find session: x"}))
}
