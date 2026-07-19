package agentconformance

import (
	"reflect"
	"testing"
)

func TestHookCommandsUsesExecutableContent(t *testing.T) {
	text := `
// ao hooks comment-only stop
{"command":"ao hooks literal user-prompt-submit"}
function hookCmd(hookName: string) {
  return ` + "`exec ao hooks template ${hookName}`" + `
}
callHookSync("session-start", {})
callHookSync("stop", {})
`
	want := []HookCommand{
		{Token: "literal", Event: "user-prompt-submit"},
		{Token: "template", Event: "session-start"},
		{Token: "template", Event: "stop"},
	}
	if got := hookCommands(text); !reflect.DeepEqual(got, want) {
		t.Fatalf("hookCommands = %#v, want %#v", got, want)
	}
}
