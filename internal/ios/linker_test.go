package ios

import (
	"reflect"
	"testing"
)

func TestRewriteSDKArgsOnlyReplacesSDKPaths(t *testing.T) {
	args := []string{
		"-syslibroot", "/sdk/iPhoneOS.sdk",
		"-F", "/sdk/iPhoneOS.sdk/System/Library/Frameworks",
		"-Wl,-rpath,/outside",
		"/outside/libFoo.a",
	}
	want := []string{
		"-syslibroot", "/cache/linker-sdk",
		"-F", "/cache/linker-sdk/System/Library/Frameworks",
		"-Wl,-rpath,/outside",
		"/outside/libFoo.a",
	}
	if got := rewriteSDKArgs(args, "/sdk/iPhoneOS.sdk", "/cache/linker-sdk"); !reflect.DeepEqual(got, want) {
		t.Fatalf("rewriteSDKArgs() = %#v, want %#v", got, want)
	}
}
