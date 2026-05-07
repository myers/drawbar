package reporter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestProcessor(debugEnabled bool) *CommandProcessor {
	rep := New(nil, 0, 0, 0)
	rep.masker = &logMasker{}
	return NewCommandProcessor(rep, debugEnabled)
}

func TestProcessLine_RegularLine(t *testing.T) {
	p := newTestProcessor(false)
	result := p.ProcessLine("hello world")
	require.NotNil(t, result)
	assert.Equal(t, "hello world", *result)
}

func TestProcessLine_PartialMatch(t *testing.T) {
	p := newTestProcessor(false)
	// Not a valid command (no closing ::)
	result := p.ProcessLine("::notacommand")
	require.NotNil(t, result)
	assert.Equal(t, "::notacommand", *result)
}

func TestProcessLine_AddMask(t *testing.T) {
	rep := New(nil, 0, 0, 0)
	rep.masker = &logMasker{}
	p := NewCommandProcessor(rep, false)

	// add-mask should drop the line
	result := p.ProcessLine("::add-mask::my-secret-token")
	assert.Nil(t, result, "add-mask line should be dropped")

	// subsequent lines should mask the value
	rep.AddLog("token is my-secret-token here")
	assert.Equal(t, "token is *** here", rep.logRows[0].Content)
}

func TestProcessLine_AddMask_ShortValue(t *testing.T) {
	rep := New(nil, 0, 0, 0)
	rep.masker = &logMasker{}
	p := NewCommandProcessor(rep, false)

	// Short values (<=3 chars) should not be masked
	result := p.ProcessLine("::add-mask::abc")
	assert.Nil(t, result)

	rep.AddLog("abc is still visible")
	assert.Equal(t, "abc is still visible", rep.logRows[0].Content)
}

func TestProcessLine_Debug_Disabled(t *testing.T) {
	p := newTestProcessor(false)
	result := p.ProcessLine("::debug::some debug info")
	assert.Nil(t, result, "debug lines should be dropped when disabled")
}

func TestProcessLine_Debug_Enabled(t *testing.T) {
	p := newTestProcessor(true)
	result := p.ProcessLine("::debug::some debug info")
	require.NotNil(t, result)
	assert.Equal(t, "::debug::some debug info", *result)
}

func TestProcessLine_Group(t *testing.T) {
	p := newTestProcessor(false)
	result := p.ProcessLine("::group::Build Step")
	require.NotNil(t, result)
	assert.Equal(t, "::group::Build Step", *result)
}

func TestProcessLine_Endgroup(t *testing.T) {
	p := newTestProcessor(false)
	result := p.ProcessLine("::endgroup::")
	require.NotNil(t, result)
	assert.Equal(t, "::endgroup::", *result)
}

func TestProcessLine_Error(t *testing.T) {
	p := newTestProcessor(false)
	result := p.ProcessLine("::error file=main.go,line=10::compile error")
	require.NotNil(t, result)
	assert.Equal(t, "::error file=main.go,line=10::compile error", *result)
}

func TestProcessLine_Warning(t *testing.T) {
	p := newTestProcessor(false)
	result := p.ProcessLine("::warning::deprecation notice")
	require.NotNil(t, result)
	assert.Equal(t, "::warning::deprecation notice", *result)
}

func TestProcessLine_Notice(t *testing.T) {
	p := newTestProcessor(false)
	result := p.ProcessLine("::notice::info message")
	require.NotNil(t, result)
	assert.Equal(t, "::notice::info message", *result)
}

func TestProcessLine_StopCommands(t *testing.T) {
	p := newTestProcessor(false)

	// stop-commands should drop the line
	result := p.ProcessLine("::stop-commands::mytoken123")
	assert.Nil(t, result, "stop-commands line should be dropped")

	// While stopped, commands should pass through as regular text
	result = p.ProcessLine("::add-mask::should-not-mask")
	require.NotNil(t, result)
	assert.Equal(t, "::add-mask::should-not-mask", *result)

	// Regular lines still pass through
	result = p.ProcessLine("hello world")
	require.NotNil(t, result)
	assert.Equal(t, "hello world", *result)

	// Resume token should drop the line and re-enable commands
	result = p.ProcessLine("::mytoken123::")
	assert.Nil(t, result, "resume token line should be dropped")

	// Commands should work again
	result = p.ProcessLine("::add-mask::now-this-works")
	assert.Nil(t, result, "add-mask should work after resume")
}

func TestProcessLine_UnknownCommand(t *testing.T) {
	p := newTestProcessor(false)
	result := p.ProcessLine("::set-output name=foo::bar")
	require.NotNil(t, result, "unknown commands pass through")
	assert.Equal(t, "::set-output name=foo::bar", *result)
}

func TestProcessLine_EmptyValue(t *testing.T) {
	p := newTestProcessor(false)
	result := p.ProcessLine("::endgroup::")
	require.NotNil(t, result)
	assert.Equal(t, "::endgroup::", *result)
}

func TestAddMask_Integration(t *testing.T) {
	rep := New(nil, 0, 0, 0)
	rep.SetSecrets([]string{"initial-secret"})
	p := NewCommandProcessor(rep, false)

	// Initial secret should be masked
	rep.AddLog("value: initial-secret")
	assert.Equal(t, "value: ***", rep.logRows[0].Content)

	// Add dynamic mask
	p.ProcessLine("::add-mask::dynamic-secret")

	// Both should now be masked
	rep.AddLog("a=initial-secret b=dynamic-secret")
	assert.Equal(t, "a=*** b=***", rep.logRows[1].Content)
}
