package app

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// immichShapedPBX reproduces the parts of
// ~/ripley-work/grail/immich/mobile/ios/Runner.xcodeproj/project.pbxproj that drive
// folder-synchronized membership: objectVersion 54 with
// preferredProjectObjectVersion 77, five PBXFileSystemSynchronizedRootGroup
// entries (Core, Sync, Schemas and Utility nested under the Runner group,
// WidgetExtension at the project root) and two exception sets - one excluding
// Info.plist from the target that owns the WidgetExtension folder, one pulling
// Mutex.swift out of the Utility folder that no target owns.
const immichShapedPBX = `// !$*UTF8*$!
{
	archiveVersion = 1;
	classes = {
	};
	objectVersion = 54;
	objects = {

/* Begin PBXFileSystemSynchronizedBuildFileExceptionSet section */
		F0B57D4D2DF764BE00DC5BCC /* Exceptions for "WidgetExtension" folder in "WidgetExtension" target */ = {
			isa = PBXFileSystemSynchronizedBuildFileExceptionSet;
			membershipExceptions = (
				Info.plist,
			);
			target = F0B57D372DF764BD00DC5BCC /* WidgetExtension */;
		};
		FE1BB4572F83196E0087DBF9 /* Exceptions for "Utility" folder in "Runner" target */ = {
			isa = PBXFileSystemSynchronizedBuildFileExceptionSet;
			membershipExceptions = (
				Mutex.swift,
			);
			target = 97C146ED1CF9000F007C117D /* Runner */;
		};
/* End PBXFileSystemSynchronizedBuildFileExceptionSet section */

/* Begin PBXFileSystemSynchronizedRootGroup section */
		B231F52D2E93A44A00BC45D1 /* Core */ = {
			isa = PBXFileSystemSynchronizedRootGroup;
			exceptions = (
			);
			path = Core;
			sourceTree = "<group>";
		};
		B2CF7F8C2DDE4EBB00744BF6 /* Sync */ = {
			isa = PBXFileSystemSynchronizedRootGroup;
			exceptions = (
			);
			path = Sync;
			sourceTree = "<group>";
		};
		F0B57D3D2DF764BD00DC5BCC /* WidgetExtension */ = {
			isa = PBXFileSystemSynchronizedRootGroup;
			exceptions = (
				F0B57D4D2DF764BE00DC5BCC /* Exceptions for "WidgetExtension" folder in "WidgetExtension" target */,
			);
			path = WidgetExtension;
			sourceTree = "<group>";
		};
		FE1BB4562F8319560087DBF9 /* Utility */ = {
			isa = PBXFileSystemSynchronizedRootGroup;
			exceptions = (
				FE1BB4572F83196E0087DBF9 /* Exceptions for "Utility" folder in "Runner" target */,
			);
			path = Utility;
			sourceTree = "<group>";
		};
		FEE084F22EC172080045228E /* Schemas */ = {
			isa = PBXFileSystemSynchronizedRootGroup;
			exceptions = (
			);
			path = Schemas;
			sourceTree = "<group>";
		};
/* End PBXFileSystemSynchronizedRootGroup section */

/* Begin PBXGroup section */
		97C146F01CF9000F007C117D /* Runner */ = {
			isa = PBXGroup;
			children = (
				FE1BB4562F8319560087DBF9 /* Utility */,
				FEE084F22EC172080045228E /* Schemas */,
				B231F52D2E93A44A00BC45D1 /* Core */,
				B2CF7F8C2DDE4EBB00744BF6 /* Sync */,
				97C147021CF9000F007C117D /* Info.plist */,
				74858FAE1ED2DC5600515810 /* AppDelegate.swift */,
			);
			path = Runner;
			sourceTree = "<group>";
		};
/* End PBXGroup section */

/* Begin PBXNativeTarget section */
		97C146ED1CF9000F007C117D /* Runner */ = {
			isa = PBXNativeTarget;
			buildConfigurationList = 97C147051CF9000F007C117D /* Build configuration list for PBXNativeTarget "Runner" */;
			buildPhases = (
				97C146EA1CF9000F007C117D /* Sources */,
			);
			buildRules = (
			);
			dependencies = (
			);
			fileSystemSynchronizedGroups = (
				B231F52D2E93A44A00BC45D1 /* Core */,
				B2CF7F8C2DDE4EBB00744BF6 /* Sync */,
				FEE084F22EC172080045228E /* Schemas */,
			);
			name = Runner;
			productName = Runner;
			productType = "com.apple.product-type.application";
		};
		F0B57D372DF764BD00DC5BCC /* WidgetExtension */ = {
			isa = PBXNativeTarget;
			buildConfigurationList = F0B57D4E2DF764BE00DC5BCC /* Build configuration list for PBXNativeTarget "WidgetExtension" */;
			buildPhases = (
				F0B57D342DF764BD00DC5BCC /* Sources */,
			);
			buildRules = (
			);
			dependencies = (
			);
			fileSystemSynchronizedGroups = (
				F0B57D3D2DF764BD00DC5BCC /* WidgetExtension */,
			);
			name = WidgetExtension;
			productName = WidgetExtension;
			productType = "com.apple.product-type.app-extension";
		};
		FAC6F88F2D287C890078CB2F /* ShareExtension */ = {
			isa = PBXNativeTarget;
			buildConfigurationList = FAC6F8A02D287C890078CB2F /* Build configuration list for PBXNativeTarget "ShareExtension" */;
			buildPhases = (
				FAC6F88C2D287C890078CB2F /* Sources */,
			);
			buildRules = (
			);
			dependencies = (
			);
			name = ShareExtension;
			productName = ShareExtension;
			productType = "com.apple.product-type.app-extension";
		};
/* End PBXNativeTarget section */

/* Begin PBXProject section */
		97C146E61CF9000F007C117D /* Project object */ = {
			isa = PBXProject;
			attributes = {
				BuildIndependentTargetsInParallel = YES;
			};
			buildConfigurationList = 97C146E91CF9000F007C117D /* Build configuration list for PBXProject "Runner" */;
			compatibilityVersion = "Xcode 9.3";
			developmentRegion = en;
			hasScannedForEncodings = 0;
			mainGroup = 97C146E51CF9000F007C117D;
			preferredProjectObjectVersion = 77;
			productRefGroup = 97C146EF1CF9000F007C117D /* Products */;
			projectDirPath = "";
			projectRoot = "";
			targets = (
				97C146ED1CF9000F007C117D /* Runner */,
				FAC6F88F2D287C890078CB2F /* ShareExtension */,
				F0B57D372DF764BD00DC5BCC /* WidgetExtension */,
			);
		};
/* End PBXProject section */
	};
	rootObject = 97C146E61CF9000F007C117D /* Project object */;
}
`

// legacyPBX is a pre-Xcode-16 project: every source is a PBXFileReference and
// there is not a single synchronized group.
const legacyPBX = `// !$*UTF8*$!
{
	archiveVersion = 1;
	classes = {
	};
	objectVersion = 46;
	objects = {

/* Begin PBXFileReference section */
		74858FAE1ED2DC5600515810 /* AppDelegate.swift */ = {isa = PBXFileReference; lastKnownFileType = sourcecode.swift; path = AppDelegate.swift; sourceTree = "<group>"; };
/* End PBXFileReference section */

/* Begin PBXNativeTarget section */
		97C146ED1CF9000F007C117D /* Runner */ = {
			isa = PBXNativeTarget;
			buildPhases = (
			);
			name = Runner;
			productName = Runner;
			productType = "com.apple.product-type.application";
		};
/* End PBXNativeTarget section */
	};
	rootObject = 97C146E61CF9000F007C117D /* Project object */;
}
`

// spotubeShapedPBX reproduces spotube's compact single-line synchronized root
// group, the form Xcode writes when every field fits on one line. It also
// carries explicitFileTypes/explicitFolders keys immich's expanded form omits.
const spotubeShapedPBX = `// !$*UTF8*$!
{
	archiveVersion = 1;
	classes = {
	};
	objectVersion = 54;
	objects = {

/* Begin PBXFileSystemSynchronizedBuildFileExceptionSet section */
		E612EC562D0F07AD0022720C /* PBXFileSystemSynchronizedBuildFileExceptionSet */ = {
			isa = PBXFileSystemSynchronizedBuildFileExceptionSet;
			membershipExceptions = (
				Info.plist,
			);
			target = E612EC382D0F07A80022720C /* HomePlayerWidgetExtension */;
		};
/* End PBXFileSystemSynchronizedBuildFileExceptionSet section */

/* Begin PBXFileSystemSynchronizedRootGroup section */
		E612EC3E2D0F07A90022720C /* HomePlayerWidget */ = {isa = PBXFileSystemSynchronizedRootGroup; exceptions = (E612EC562D0F07AD0022720C /* PBXFileSystemSynchronizedBuildFileExceptionSet */, ); explicitFileTypes = {}; explicitFolders = (); path = HomePlayerWidget; sourceTree = "<group>"; };
/* End PBXFileSystemSynchronizedRootGroup section */

/* Begin PBXNativeTarget section */
		E612EC382D0F07A80022720C /* HomePlayerWidgetExtension */ = {
			isa = PBXNativeTarget;
			buildPhases = (
			);
			fileSystemSynchronizedGroups = (
				E612EC3E2D0F07A90022720C /* HomePlayerWidget */,
			);
			name = HomePlayerWidgetExtension;
			productName = HomePlayerWidgetExtension;
			productType = "com.apple.product-type.app-extension";
		};
/* End PBXNativeTarget section */
	};
	rootObject = 97C146E61CF9000F007C117D /* Project object */;
}
`

// writeFiles creates every named file under root with a byte of content.
func writeFiles(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, name := range names {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte("// "+name+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

// immichShapedTree lays out the real immich ios/ directory contents that the
// synchronized groups point at.
func immichShapedTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFiles(t, root,
		"Runner/AppDelegate.swift",
		"Runner/Info.plist",
		"Runner/Core/ImmichPlugin.swift",
		"Runner/Core/NetworkApiImpl.swift",
		"Runner/Core/URLSessionManager.swift",
		"Runner/Sync/MessagesImpl.swift",
		"Runner/Sync/PHAssetExtensions.swift",
		"Runner/Sync/PHAssetResourceExtensions.swift",
		"Runner/Schemas/Constants.swift",
		"Runner/Schemas/Store.swift",
		"Runner/Schemas/Tables.swift",
		"Runner/Utility/Mutex.swift",
		"Runner/Utility/Scratchpad.swift",
		"WidgetExtension/ImageEntry.swift",
		"WidgetExtension/ImageWidgetView.swift",
		"WidgetExtension/ImmichAPI.swift",
		"WidgetExtension/Info.plist",
		"WidgetExtension/UIImage+Resize.swift",
		"WidgetExtension/WidgetBundle.swift",
		"WidgetExtension/WidgetExtension.entitlements",
		"WidgetExtension/widgets/MemoryWidget.swift",
		"WidgetExtension/widgets/RandomWidget.swift",
		"WidgetExtension/Assets.xcassets/Contents.json",
		"WidgetExtension/Assets.xcassets/AppIcon.appiconset/Contents.json",
	)
	return root
}

// relativeMembers turns absolute member paths back into slash paths relative to
// root so expectations read like the pbxproj they came from.
func relativeMembers(t *testing.T, root string, members []string) []string {
	t.Helper()
	out := make([]string, 0, len(members))
	for _, member := range members {
		rel, err := filepath.Rel(root, member)
		if err != nil {
			t.Fatalf("rel %s: %v", member, err)
		}
		if strings.HasPrefix(rel, "..") {
			t.Fatalf("member %s escapes root %s", member, root)
		}
		out = append(out, filepath.ToSlash(rel))
	}
	sort.Strings(out)
	return out
}

func mustParseObjects(t *testing.T, text string) map[string]pbxObject {
	t.Helper()
	objects, err := parsePBXObjects(text)
	if err != nil {
		t.Fatalf("parsePBXObjects: %v", err)
	}
	return objects
}

func mustTarget(t *testing.T, objects map[string]pbxObject, name string) xcodeTarget {
	t.Helper()
	for _, object := range objects {
		fields := pbxEntries(object.Body)
		if pbxEntryValue(fields, "isa") != "PBXNativeTarget" || pbxEntryValue(fields, "name") != name {
			continue
		}
		return xcodeTarget{
			ID:                     object.ID,
			Name:                   name,
			BuildConfigurationList: pbxEntryID(fields, "buildConfigurationList"),
			BuildPhases:            pbxEntryIDs(fields, "buildPhases"),
		}
	}
	t.Fatalf("target %q not found", name)
	return xcodeTarget{}
}

func TestTargetSynchronizedSourcesResolvesNestedGroupMembership(t *testing.T) {
	root := immichShapedTree(t)
	objects := mustParseObjects(t, immichShapedPBX)
	target := mustTarget(t, objects, "Runner")

	members, err := targetSynchronizedSources(objects, target, root)
	if err != nil {
		t.Fatalf("targetSynchronizedSources: %v", err)
	}
	got := relativeMembers(t, root, members)
	want := []string{
		"Runner/Core/ImmichPlugin.swift",
		"Runner/Core/NetworkApiImpl.swift",
		"Runner/Core/URLSessionManager.swift",
		"Runner/Schemas/Constants.swift",
		"Runner/Schemas/Store.swift",
		"Runner/Schemas/Tables.swift",
		"Runner/Sync/MessagesImpl.swift",
		"Runner/Sync/PHAssetExtensions.swift",
		"Runner/Sync/PHAssetResourceExtensions.swift",
		"Runner/Utility/Mutex.swift",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Runner members\n got %v\nwant %v", got, want)
	}
	for _, member := range members {
		if !filepath.IsAbs(member) {
			t.Fatalf("member %q is not absolute", member)
		}
	}
}

// Runner does not own the Utility folder; only the membership exception pulls
// Mutex.swift in, so its sibling must stay out even though it sits in the same
// synchronized folder.
func TestTargetSynchronizedSourcesInclusionOnlyExceptionExcludesSiblings(t *testing.T) {
	root := immichShapedTree(t)
	objects := mustParseObjects(t, immichShapedPBX)
	target := mustTarget(t, objects, "Runner")

	members, err := targetSynchronizedSources(objects, target, root)
	if err != nil {
		t.Fatalf("targetSynchronizedSources: %v", err)
	}
	got := relativeMembers(t, root, members)
	if !contains(got, "Runner/Utility/Mutex.swift") {
		t.Fatalf("inclusion exception dropped Mutex.swift: %v", got)
	}
	if contains(got, "Runner/Utility/Scratchpad.swift") {
		t.Fatalf("inclusion-only exception leaked a sibling file: %v", got)
	}
	if contains(got, "Runner/AppDelegate.swift") || contains(got, "Runner/Info.plist") {
		t.Fatalf("synchronized resolution picked up files outside the groups: %v", got)
	}
}

func TestTargetSynchronizedSourcesHonoursMembershipExclusion(t *testing.T) {
	root := immichShapedTree(t)
	objects := mustParseObjects(t, immichShapedPBX)
	target := mustTarget(t, objects, "WidgetExtension")

	members, err := targetSynchronizedSources(objects, target, root)
	if err != nil {
		t.Fatalf("targetSynchronizedSources: %v", err)
	}
	got := relativeMembers(t, root, members)
	want := []string{
		"WidgetExtension/Assets.xcassets",
		"WidgetExtension/ImageEntry.swift",
		"WidgetExtension/ImageWidgetView.swift",
		"WidgetExtension/ImmichAPI.swift",
		"WidgetExtension/UIImage+Resize.swift",
		"WidgetExtension/WidgetBundle.swift",
		"WidgetExtension/WidgetExtension.entitlements",
		"WidgetExtension/widgets/MemoryWidget.swift",
		"WidgetExtension/widgets/RandomWidget.swift",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WidgetExtension members\n got %v\nwant %v", got, want)
	}
}

// The Runner group's own Info.plist exclusion belongs to WidgetExtension only:
// a target that shares neither ownership nor an exception gets nothing.
func TestTargetSynchronizedSourcesTargetWithoutGroupsGetsNothing(t *testing.T) {
	root := immichShapedTree(t)
	objects := mustParseObjects(t, immichShapedPBX)
	target := mustTarget(t, objects, "ShareExtension")

	members, err := targetSynchronizedSources(objects, target, root)
	if err != nil {
		t.Fatalf("targetSynchronizedSources: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("ShareExtension owns no synchronized group, got %v", members)
	}
}

func TestTargetSynchronizedSourcesLegacyProjectIsNoOp(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "Runner/AppDelegate.swift")
	objects := mustParseObjects(t, legacyPBX)
	target := mustTarget(t, objects, "Runner")

	groups, err := parseSynchronizedGroups(objects)
	if err != nil {
		t.Fatalf("parseSynchronizedGroups: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("legacy project reported %d synchronized groups", len(groups))
	}
	members, err := targetSynchronizedSources(objects, target, root)
	if err != nil {
		t.Fatalf("targetSynchronizedSources: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("legacy project yielded members %v", members)
	}
}

// spotube writes its synchronized root group as a single line; the parser must
// see it exactly like immich's expanded block.
func TestTargetSynchronizedSourcesCompactSingleLineGroup(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root,
		"HomePlayerWidget/HomePlayerWidget.swift",
		"HomePlayerWidget/Info.plist",
		"HomePlayerWidget/Assets.xcassets/Contents.json",
	)
	objects := mustParseObjects(t, spotubeShapedPBX)

	groups, err := parseSynchronizedGroups(objects)
	if err != nil {
		t.Fatalf("parseSynchronizedGroups: %v", err)
	}
	group, ok := groups["E612EC3E2D0F07A90022720C"]
	if !ok {
		t.Fatalf("compact group not parsed, got %v", groups)
	}
	if group.Path != "HomePlayerWidget" || group.SourceTree != "<group>" {
		t.Fatalf("compact group fields: path=%q sourceTree=%q", group.Path, group.SourceTree)
	}

	target := mustTarget(t, objects, "HomePlayerWidgetExtension")
	members, err := targetSynchronizedSources(objects, target, root)
	if err != nil {
		t.Fatalf("targetSynchronizedSources: %v", err)
	}
	got := relativeMembers(t, root, members)
	want := []string{
		"HomePlayerWidget/Assets.xcassets",
		"HomePlayerWidget/HomePlayerWidget.swift",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compact group members\n got %v\nwant %v", got, want)
	}
}

// explicitFolders names directories Xcode hands to the build as a single opaque
// member instead of synchronizing their contents.
func TestTargetSynchronizedSourcesExplicitFoldersStayOpaque(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root,
		"Sources/main.swift",
		"Sources/Resources/data.json",
		"Sources/Resources/nested/more.json",
		"Sources/.hidden/secret.swift",
	)
	pbx := `// !$*UTF8*$!
{
	objectVersion = 77;
	objects = {
		AAAAAAAAAAAAAAAAAAAAAAAA /* Sources */ = {
			isa = PBXFileSystemSynchronizedRootGroup;
			explicitFolders = (
				Resources,
			);
			path = Sources;
			sourceTree = "<group>";
		};
		BBBBBBBBBBBBBBBBBBBBBBBB /* App */ = {
			isa = PBXNativeTarget;
			fileSystemSynchronizedGroups = (
				AAAAAAAAAAAAAAAAAAAAAAAA /* Sources */,
			);
			name = App;
			productType = "com.apple.product-type.application";
		};
	};
}
`
	objects := mustParseObjects(t, pbx)
	target := mustTarget(t, objects, "App")
	members, err := targetSynchronizedSources(objects, target, root)
	if err != nil {
		t.Fatalf("targetSynchronizedSources: %v", err)
	}
	got := relativeMembers(t, root, members)
	want := []string{"Sources/Resources", "Sources/main.swift"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("explicitFolders members\n got %v\nwant %v", got, want)
	}
}

func TestProjectObjectVersion(t *testing.T) {
	cases := []struct {
		name string
		pbx  string
		want int
	}{
		{"modern project prefers 77 over compatibility 54", immichShapedPBX, 77},
		{"legacy project reports its objectVersion", legacyPBX, 46},
		{"missing keys report zero", "// !$*UTF8*$!\n{\n\tarchiveVersion = 1;\n}\n", 0},
		{"quoted value", "objectVersion = \"70\";\n", 70},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectObjectVersion(tc.pbx); got != tc.want {
				t.Fatalf("projectObjectVersion = %d, want %d", got, tc.want)
			}
		})
	}
}

// A synchronized group whose folder is missing is a hard error for the target
// that owns it: silently compiling zero sources is how a Flutter app links
// without its plugin glue.
func TestTargetSynchronizedSourcesMissingFolderFails(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, "Runner/AppDelegate.swift")
	objects := mustParseObjects(t, immichShapedPBX)
	target := mustTarget(t, objects, "Runner")

	_, err := targetSynchronizedSources(objects, target, root)
	if err == nil {
		t.Fatal("expected an error for a synchronized group with no folder on disk")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
