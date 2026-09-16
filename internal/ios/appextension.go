package ios

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ripley/internal/toolchain"
)

// BuildAppExtension compiles an app-extension target's own sources and links
// them into the Mach-O that lives at <App>.app/PlugIns/<Name>.appex/<Name>.
//
// Two things differ from the application host link, and both are load-bearing:
//
//   - The entry point is `_NSExtensionMain`, not `_main`. An `.appex` is spawned
//     as its own process by pluginkit, so it is a normal MH_EXECUTE that needs
//     LC_MAIN and a dyld load command; linking it with `-bundle` produces an
//     MH_BUNDLE with neither and iOS could never launch it. Apple's own store
//     validation rejects a missing entry point with ITMS-90898.
//   - It carries an extra `@executable_path/../../Frameworks` rpath, because the
//     host app's Frameworks directory is two levels above `PlugIns/<Name>.appex`.
//
// Flutter is deliberately not force-linked: a widget extension links WidgetKit
// and SwiftUI, and an extension that genuinely needs Flutter (zulip's
// NotificationService) receives it through pluginFrameworks.
func BuildAppExtension(tc toolchain.Toolchain, out, work, minOS, moduleName, bridgingHeader string, sources []string, extra HostInputs, pluginFrameworks []string) (string, error) {
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}

	swiftSources, cSources := splitHostSources(sources)
	if len(swiftSources) == 0 && len(cSources) == 0 {
		return "", fmt.Errorf("app extension %s has no compilable sources", moduleName)
	}

	sdk, err := tc.IOSSDK()
	if err != nil {
		return "", err
	}
	sdkVersion, err := tc.IOSSDKVersion()
	if err != nil {
		return "", err
	}
	search, err := frameworkSearch(tc)
	if err != nil {
		return "", err
	}

	var objects []string
	if len(swiftSources) > 0 {
		swiftc, err := swiftCompiler(tc)
		if err != nil {
			return "", err
		}
		flags, err := commonSwiftFlags(tc, minOS)
		if err != nil {
			return "", err
		}
		object := filepath.Join(work, "ext_swift.o")
		args := append(append([]string{}, flags...), search...)
		for _, dir := range extra.HeaderSearchPaths {
			args = append(args, "-Xcc", "-I"+dir)
		}
		for _, dir := range extra.ModuleSearchPaths {
			args = append(args, "-I", dir, "-Xcc", "-I"+dir)
		}
		for _, path := range extra.ModuleMaps {
			args = append(args, "-Xcc", "-fmodule-map-file="+path)
		}
		for _, plugin := range extra.MacroPlugins {
			args = append(args, "-load-plugin-executable", plugin)
		}
		// #Preview/#Previewable are Xcode-canvas macros declared by the SDK but
		// implemented by a plugin the Linux toolchain lacks; a stub that expands
		// them to nothing is correct for a device build.
		if preview, err := previewsMacrosPlugin(tc, swiftSources); err != nil {
			return "", err
		} else if preview != "" {
			args = append(args, "-load-plugin-executable", preview)
		}
		args = append(args, hostSwiftFrameworkSearchArgs(pluginFrameworks)...)
		args = append(args, swiftSources...)
		args = append(args, "-module-name", moduleName, "-o", object)
		if bridgingHeader != "" && fileExists(bridgingHeader) {
			args = append(args, "-import-objc-header", bridgingHeader)
		}
		if err := run("", nil, swiftc, args...); err != nil {
			return "", err
		}
		objects = append(objects, object)
	}
	// An ObjC source in an extension may `#import <Flutter/Flutter.h>`, so the
	// framework search dirs must reach clang as well as swiftc.
	cObjects, err := compileHostSources(tc, cSources, hostIncludeDirs(extra), frameworkParentDirs(pluginFrameworks), extra.ModuleMaps, work, minOS)
	if err != nil {
		return "", err
	}
	objects = append(objects, cObjects...)

	args := []string{
		"-arch", "arm64", "-fixup_chains",
		"-platform_version", "ios", minOS, sdkVersion,
		"-syslibroot", sdk,
		"-e", "_NSExtensionMain",
		"-o", out,
	}
	args = append(args, objects...)
	for _, object := range extra.Objects {
		if object != "" {
			args = append(args, object)
		}
	}
	for _, framework := range pluginFrameworks {
		if strings.HasSuffix(framework, ".framework") {
			args = append(args, "-framework", strings.TrimSuffix(filepath.Base(framework), ".framework"), "-F", filepath.Dir(framework))
			continue
		}
		// A bare name is a system framework the extension declares, e.g.
		// WidgetKit or UserNotifications.
		args = append(args, "-framework", framework)
	}
	for _, framework := range extra.LinkerFrameworks {
		args = append(args, "-framework", framework)
	}
	for _, library := range extra.LinkerLibraries {
		args = append(args, "-l"+library)
	}
	args = append(args, extra.LinkerFlags...)
	args = append(args,
		"-framework", "UIKit", "-framework", "Foundation",
		"-lswiftCore", "-lswiftFoundation", "-lswiftDarwin", "-lswiftObjectiveC", "-lswiftDispatch", "-lswiftCoreFoundation",
		"-lswiftCompatibility56", "-lswiftCompatibilityPacks", "-lswiftCompatibilityConcurrency",
	)
	args = append(args, search...)
	libs, err := libSearch(tc)
	if err != nil {
		return "", err
	}
	args = append(args, libs...)
	swiftIOSLibs, err := tc.SwiftIOSLibs()
	if err != nil {
		return "", err
	}
	if runtimeLib := filepath.Join(swiftIOSLibs, "libclang_rt.ios.a"); fileExists(runtimeLib) {
		args = append(args, runtimeLib)
	}
	args = append(args, swiftExecutableRPathArgs(
		"@executable_path/Frameworks",
		"@executable_path/../../Frameworks",
	)...)
	args = append(args, "-lSystem")
	if err := runLD64(tc, args...); err != nil {
		return "", err
	}
	if err := os.Chmod(out, 0o755); err != nil {
		return "", err
	}
	return out, nil
}
