package app

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestModuleMapHeaderSearchPathsExposeCocoaPodsPublicHeaders(t *testing.T) {
	root := t.TempDir()
	firebase := filepath.Join(root, "Pods", "Headers", "Public", "FirebaseCore", "FirebaseCore.modulemap")
	storage := filepath.Join(root, "Pods", "Headers", "Public", "storage_info", "storage_info.modulemap")
	got := moduleMapHeaderSearchPaths([]string{firebase, storage, firebase, ""})
	want := []string{
		filepath.Join(root, "Pods", "Headers", "Public"),
		filepath.Join(root, "Pods", "Headers", "Public", "FirebaseCore"),
		filepath.Join(root, "Pods", "Headers", "Public", "storage_info"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("module-map header paths = %#v, want %#v", got, want)
	}
}
