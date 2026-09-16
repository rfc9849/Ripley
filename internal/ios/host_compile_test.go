package ios

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestHostCSourceModuleFlagsForObjC(t *testing.T) {
	maps := []string{"/pods/A.modulemap", "/pods/B.modulemap", "/pods/A.modulemap"}
	want := []string{
		"-fobjc-arc", "-fmodules",
		"-fmodule-map-file=/pods/A.modulemap",
		"-fmodule-map-file=/pods/B.modulemap",
	}
	for _, source := range []string{"GeneratedPluginRegistrant.m", "bridge.mm"} {
		if got := hostCSourceModuleFlags(source, maps); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s flags = %#v, want %#v", source, got, want)
		}
	}
}

func TestHostCSourceModuleFlagsSkipPureCAndCXX(t *testing.T) {
	for _, source := range []string{"shim.c", "native.cc", "native.cpp", "native.cxx"} {
		if got := hostCSourceModuleFlags(source, []string{"/pods/A.modulemap"}); len(got) != 0 {
			t.Fatalf("%s unexpectedly got ObjC module flags: %#v", source, got)
		}
	}
}

func TestClangModuleMapsPreferPublicMapByDeclaredModuleName(t *testing.T) {
	root := t.TempDir()
	firebase := filepath.Join(root, "Pods", "Headers", "Public", "FirebaseMessaging")
	flutter := filepath.Join(root, "Pods", "Headers", "Public", "Flutter")
	buildMirrorDir := filepath.Join(root, "build", "Release-iphoneos", "FirebaseMessaging")
	for _, dir := range []string{firebase, flutter, buildMirrorDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	publicMap := filepath.Join(firebase, "FirebaseMessaging.modulemap")
	mirrorMap := filepath.Join(buildMirrorDir, "FirebaseMessaging.modulemap")
	flutterMap := filepath.Join(flutter, "Flutter.modulemap")
	uniqueMap := filepath.Join(root, "Explicit.modulemap")
	for path, body := range map[string]string{
		publicMap:  "module FirebaseMessaging { export * }\n",
		mirrorMap:  "module FirebaseMessaging { export * }\n",
		flutterMap: "module Flutter { export * }\n",
		uniqueMap:  "module Explicit { export * }\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := clangModuleMaps([]string{firebase, flutter}, []string{mirrorMap, uniqueMap, publicMap})
	want := []string{uniqueMap, publicMap}
	// clangModuleMaps returns a deterministic sorted list after resolving which
	// declaration wins, so compare sorted expected paths too.
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("module maps = %#v, want %#v", got, want)
	}
}

func TestHostSwiftFrameworkSearchArgsExposePluginFrameworkModules(t *testing.T) {
	frameworks := []string{
		"/build/Sentry/Sentry.framework",
		"/build/workmanager/workmanager_apple.framework",
		"UIKit",
		"/build/Sentry/Sentry.framework",
	}
	want := []string{"-F", "/build/Sentry", "-F", "/build/workmanager"}
	if got := hostSwiftFrameworkSearchArgs(frameworks); !reflect.DeepEqual(got, want) {
		t.Fatalf("Swift plugin framework search args = %#v, want %#v", got, want)
	}
}
