package app

import "testing"

func TestRewriteDirectFlutterSceneDelegate(t *testing.T) {
	entry := map[string]any{
		"UISceneDelegateClassName": "FlutterSceneDelegate",
		"UISceneStoryboardFile":    "Main",
	}
	info := map[string]any{
		"UIApplicationSceneManifest": map[string]any{
			"UISceneConfigurations": map[string]any{
				"UIWindowSceneSessionRoleApplication": []any{entry},
			},
		},
	}

	rewriteDirectFlutterSceneDelegate(info, "Runner")
	if got := entry["UISceneDelegateClassName"]; got != "Runner.SceneDelegate" {
		t.Fatalf("delegate = %v, want Runner.SceneDelegate", got)
	}
	if got := entry["UISceneStoryboardFile"]; got != "Main" {
		t.Fatalf("rewrite unexpectedly changed storyboard = %v", got)
	}
}

func TestRewriteDirectFlutterSceneDelegatePreservesCustomDelegate(t *testing.T) {
	entry := map[string]any{
		"UISceneDelegateClassName": "Runner.CustomSceneDelegate",
		"UISceneStoryboardFile":    "Main",
	}
	info := map[string]any{
		"UIApplicationSceneManifest": map[string]any{
			"UISceneConfigurations": map[string]any{
				"UIWindowSceneSessionRoleApplication": []any{entry},
			},
		},
	}

	rewriteDirectFlutterSceneDelegate(info, "Runner")
	if got := entry["UISceneDelegateClassName"]; got != "Runner.CustomSceneDelegate" {
		t.Fatalf("custom delegate changed to %v", got)
	}
}
