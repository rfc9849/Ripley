package ios

import (
	"bytes"
	"debug/macho"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	"howett.net/plist"

	"ripley/internal/toolchain"
)

type Plugin struct {
	Name        string
	Path        string
	IOSDir      string
	PluginClass string
	NativeBuild bool
}

// PluginBuild is the result of building an application's native plugin
// dependencies.
//
// ModuleSearchPaths and ModuleMaps carry the loose CocoaPods module view used
// by classic static-library products, so an AppDelegate that says
// `import <PodModule>` can resolve both Swift-backed and Objective-C-only pods.
// A pod's Swift module needs both halves:
//
//   - ModuleSearchPaths holds the directory with the compiled `.swiftmodule`
//     (`-I`), which is what the Swift importer needs.
//   - ModuleMaps holds CocoaPods' public modulemap declaring the module's ObjC
//     half (`-Xcc -fmodule-map-file=`). It is required, not optional: the
//     module map is an umbrella over `Pods/Headers/Public/<Pod>`, whose headers
//     are only reachable relative to that map, and nothing under
//     `Pods/Headers/Public` is named `module.modulemap`, so clang cannot
//     discover it from a header search path.
//
// The build deliberately does not also write a `module.modulemap` beside the
// `.swiftmodule`: clang would auto-discover it from the `-I` entry and then
// reject the explicit map with "umbrella for module 'X' already covers this
// directory". Exactly one of the two mechanisms may be in play.

// PodModule describes one built CocoaPods target's module so a downstream
// compile — an app extension importing the pod — can resolve and link it.
type PodModule struct {
	// Name is the pod target name; ModuleName is the module name an import
	// statement uses. They usually coincide.
	Name       string
	ModuleName string
	// ModuleDir holds the emitted .swiftmodule; ModuleMap is the CocoaPods
	// public modulemap for the module's ObjC half. Either may be empty for a
	// pod that produced no Swift module or no public map.
	ModuleDir string
	ModuleMap string
	// Archive is the built static library a dependent target links when the pod
	// is built as a classic CocoaPods static-library target. Framework is the
	// per-pod framework product for use_frameworks! (dynamic or static).
	Archive         string
	Framework       string
	StaticFramework bool
	// LinkerFlags are target-specific flags that must survive until the final
	// executable link for static framework products (for example cargokit's
	// -force_load of a generated Rust archive). Dynamic frameworks consume the
	// same flags while linking their dylib and leave this empty.
	LinkerFlags []string
	// Dependencies are pod target names this module needs at compile and link
	// time; a dependent must see the whole closure.
	Dependencies []string
}

type PluginBuild struct {
	// Frameworks are compile/link inputs. EmbeddedFrameworks is the runtime
	// subset copied into <App>.app/Frameworks. Static CocoaPods frameworks are
	// intentionally present only in Frameworks.
	Frameworks         []string
	EmbeddedFrameworks []string
	// Objects are native plugin objects linked directly into the application
	// host. Flutter's SwiftPM plugin integration is static, so package-only
	// plugins belong here rather than being wrapped in synthetic dylibs.
	Objects           []string
	Resources         []string
	RegistrantObject  string
	ModuleSearchPaths []string
	ModuleMaps        []string
	// LinkerFlags are additional raw ld flags required at the final executable
	// link. Static CocoaPods framework targets use this for pod-target settings
	// such as -force_load; dynamic framework targets consume those flags earlier.
	LinkerFlags      []string
	LinkerLibraries  []string
	LinkerFrameworks []string
	// FrameworkLinkage is "dynamic" or "static" when CocoaPods use_frameworks!
	// selected framework products. Empty means the classic/static-library path.
	FrameworkLinkage string
	Plugins          []Plugin
	// PodModules indexes every built pod target by target name so targets
	// outside the application — app extensions — can resolve and link the pod
	// modules they import.
	PodModules map[string]PodModule
}

type pluginDependencyFile struct {
	Plugins struct {
		IOS []struct {
			Name        string `json:"name"`
			Path        string `json:"path"`
			Class       string `json:"class"`
			PluginClass string `json:"plugin_class"`
			NativeBuild *bool  `json:"native_build"`
		} `json:"ios"`
	} `json:"plugins"`
}

// DiscoverPlugins lists the project's iOS plugins. packageConfigPath is the
// package config that governs projectRoot; under pub workspace resolution it
// lives at the workspace root rather than below the member, so the caller
// resolves it. An empty value falls back to the project-local path.
func DiscoverPlugins(projectRoot, packageConfigPath string) ([]Plugin, error) {
	var plugins []Plugin
	seen := make(map[string]bool)
	dependenciesPath := filepath.Join(projectRoot, ".flutter-plugins-dependencies")
	if data, err := os.ReadFile(dependenciesPath); err == nil {
		var deps pluginDependencyFile
		if err := json.Unmarshal(data, &deps); err != nil {
			return nil, fmt.Errorf("decode %s: %w", dependenciesPath, err)
		}
		for _, entry := range deps.Plugins.IOS {
			path, err := filepath.Abs(entry.Path)
			if err != nil {
				return nil, err
			}
			if seen[entry.Name] || !dirExists(path) {
				continue
			}
			iosDir := findPluginIOSDir(path)
			if iosDir == "" {
				continue
			}
			pubspecInfo, hasPubspecInfo, err := pubspecPluginInfo(path)
			if err != nil {
				return nil, err
			}
			native := true
			if entry.NativeBuild != nil {
				native = *entry.NativeBuild
			} else if hasPubspecInfo {
				native = pubspecInfo.NativeBuild
			}
			class := entry.PluginClass
			if class == "" {
				class = entry.Class
			}
			if class == "" && hasPubspecInfo {
				class = pubspecInfo.PluginClass
			}
			plugins = append(plugins, Plugin{Name: entry.Name, Path: path, IOSDir: iosDir, PluginClass: class, NativeBuild: native})
			seen[entry.Name] = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	packageDirs, err := packageConfigDirectories(projectRoot, packageConfigPath)
	if err != nil {
		return nil, err
	}
	for _, pkgDir := range packageDirs {
		info, ok, err := pubspecPluginInfo(pkgDir)
		if err != nil {
			return nil, err
		}
		if !ok || !info.NativeBuild {
			continue
		}
		name, err := pubspecName(pkgDir)
		if err != nil {
			continue
		}
		if name == "" {
			name = filepath.Base(pkgDir)
		}
		if seen[name] {
			continue
		}
		iosDir := findPluginIOSDir(pkgDir)
		if iosDir == "" {
			continue
		}
		plugins = append(plugins, Plugin{Name: name, Path: pkgDir, IOSDir: iosDir, PluginClass: info.PluginClass, NativeBuild: info.NativeBuild})
		seen[name] = true
	}
	return plugins, nil
}

func findPluginIOSDir(root string) string {
	for _, name := range []string{"ios", "darwin"} {
		path := filepath.Join(root, name)
		if dirExists(path) {
			return path
		}
	}
	return ""
}

type pluginPubspecInfo struct {
	PluginClass string
	NativeBuild bool
}

func pubspecPluginInfo(pkgDir string) (pluginPubspecInfo, bool, error) {
	data, err := os.ReadFile(filepath.Join(pkgDir, "pubspec.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return pluginPubspecInfo{}, false, nil
	}
	if err != nil {
		return pluginPubspecInfo{}, false, err
	}
	var doc struct {
		Flutter struct {
			Plugin struct {
				FFIPlugin bool                 `yaml:"ffiPlugin"`
				Platforms map[string]yaml.Node `yaml:"platforms"`
			} `yaml:"plugin"`
		} `yaml:"flutter"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return pluginPubspecInfo{}, false, nil
	}
	node, ok := doc.Flutter.Plugin.Platforms["ios"]
	if !ok {
		node, ok = doc.Flutter.Plugin.Platforms["darwin"]
	}
	if !ok {
		return pluginPubspecInfo{}, false, nil
	}
	var platform struct {
		PluginClass string `yaml:"pluginClass"`
		FFIPlugin   bool   `yaml:"ffiPlugin"`
	}
	if node.Kind == yaml.MappingNode {
		if err := node.Decode(&platform); err != nil {
			return pluginPubspecInfo{}, false, err
		}
	}
	return pluginPubspecInfo{
		PluginClass: platform.PluginClass,
		NativeBuild: platform.PluginClass != "" || platform.FFIPlugin || doc.Flutter.Plugin.FFIPlugin,
	}, true, nil
}

func packageConfigDirectories(projectRoot, packageConfigPath string) ([]string, error) {
	path := packageConfigPath
	if path == "" {
		path = filepath.Join(projectRoot, ".dart_tool", "package_config.json")
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var config struct {
		Packages []struct {
			RootURI string `json:"rootUri"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(config.Packages))
	for _, pkg := range config.Packages {
		if strings.HasPrefix(pkg.RootURI, "file://") {
			u, err := url.Parse(pkg.RootURI)
			if err != nil {
				return nil, err
			}
			decoded, err := url.PathUnescape(u.Path)
			if err != nil {
				return nil, err
			}
			out = append(out, decoded)
			continue
		}
		decoded, err := url.PathUnescape(pkg.RootURI)
		if err != nil {
			return nil, err
		}
		abs, err := filepath.Abs(filepath.Join(filepath.Dir(path), filepath.FromSlash(decoded)))
		if err != nil {
			return nil, err
		}
		out = append(out, abs)
	}
	return out, nil
}

func pubspecName(pkgDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(pkgDir, "pubspec.yaml"))
	if err != nil {
		return "", err
	}
	var spec struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return "", err
	}
	return spec.Name, nil
}

type podspecInfo struct {
	Name               string
	SourceFiles        []string
	PublicHeaders      []string
	Frameworks         []string
	Libraries          []string
	VendoredFrameworks []string
	VendoredLibraries  []string
	Dependencies       []string
	Resources          []string
	SwiftVersion       string
	DeploymentTarget   string
	ModuleName         string
}

var quotedStringRE = regexp.MustCompile(`['"]([^'"]+)['"]`)

func parsePodspec(path string) (podspecInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return podspecInfo{}, err
	}
	text := string(data)
	info := podspecInfo{Name: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), SwiftVersion: "5.0"}
	info.ModuleName = info.Name
	stringsFor := func(key string) []string {
		re := regexp.MustCompile(`(?s)s\.(?:ios\.)?` + regexp.QuoteMeta(key) + `\s*=\s*(\[.*?\]|'[^']*'|"[^"]*"|\{.*?\})`)
		var out []string
		for _, match := range re.FindAllStringSubmatch(text, -1) {
			for _, quoted := range quotedStringRE.FindAllStringSubmatch(match[1], -1) {
				out = append(out, quoted[1])
			}
		}
		return out
	}
	info.SourceFiles = stringsFor("source_files")
	info.PublicHeaders = stringsFor("public_header_files")
	info.Resources = stringsFor("resources")
	info.VendoredFrameworks = stringsFor("vendored_frameworks")
	info.VendoredLibraries = stringsFor("vendored_libraries")
	info.Frameworks = uniqueStrings(append(stringsFor("framework"), stringsFor("frameworks")...))
	info.Libraries = append(stringsFor("library"), stringsFor("libraries")...)
	dependencyRE := regexp.MustCompile(`(?m)s\.dependency\s+['\"]([^'\"]+)['\"]`)
	for _, match := range dependencyRE.FindAllStringSubmatch(text, -1) {
		info.Dependencies = append(info.Dependencies, match[1])
	}
	if m := regexp.MustCompile(`s\.swift_version\s*=\s*['"]([^'"]+)`).FindStringSubmatch(text); m != nil {
		info.SwiftVersion = m[1]
	}
	platform := regexp.MustCompile(`s\.platform\s*=\s*:ios,\s*['"]([^'"]+)`).FindStringSubmatch(text)
	if platform == nil {
		platform = regexp.MustCompile(`s\.ios\.deployment_target\s*=\s*['"]([^'"]+)`).FindStringSubmatch(text)
	}
	if platform != nil {
		info.DeploymentTarget = platform[1]
	}
	if m := regexp.MustCompile(`s\.module_name\s*=\s*['"]([^'"]+)`).FindStringSubmatch(text); m != nil {
		info.ModuleName = m[1]
	}
	return info, nil
}

func expandBraces(pattern string) []string {
	start := strings.IndexByte(pattern, '{')
	if start < 0 {
		return []string{pattern}
	}
	endRel := strings.IndexByte(pattern[start:], '}')
	if endRel < 0 {
		return []string{pattern}
	}
	end := start + endRel
	var out []string
	for _, alt := range strings.Split(pattern[start+1:end], ",") {
		out = append(out, expandBraces(pattern[:start]+alt+pattern[end+1:])...)
	}
	return out
}

func globSources(base string, patterns []string) ([]string, error) {
	matches := make(map[string]bool)
	for _, pattern := range patterns {
		for _, expanded := range expandBraces(filepath.ToSlash(pattern)) {
			re, err := globPatternRegexp(expanded)
			if err != nil {
				return nil, err
			}
			err = filepath.WalkDir(base, func(path string, d os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if d.IsDir() {
					return nil
				}
				rel, err := filepath.Rel(base, path)
				if err != nil {
					return err
				}
				if re.MatchString(filepath.ToSlash(rel)) && nativeSourceExtension(filepath.Ext(path)) {
					matches[path] = true
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	out := make([]string, 0, len(matches))
	for path := range matches {
		out = append(out, path)
	}
	sort.Strings(out)
	return out, nil
}

func globPatternRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 3
				} else {
					b.WriteString(".*")
					i += 2
				}
			} else {
				b.WriteString("[^/]*")
				i++
			}
		case '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
			i++
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

func nativeSourceExtension(ext string) bool {
	switch ext {
	case ".m", ".mm", ".c", ".cc", ".cpp", ".swift", ".h", ".hpp", ".hh":
		return true
	default:
		return false
	}
}

func globPaths(base string, patterns []string) ([]string, error) {
	matches := make(map[string]bool)
	for _, pattern := range patterns {
		for _, expanded := range expandBraces(filepath.ToSlash(pattern)) {
			re, err := globPatternRegexp(expanded)
			if err != nil {
				return nil, err
			}
			err = filepath.WalkDir(base, func(path string, d os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if path == base {
					return nil
				}
				rel, err := filepath.Rel(base, path)
				if err != nil {
					return err
				}
				if re.MatchString(filepath.ToSlash(rel)) {
					matches[path] = true
					if d.IsDir() {
						return filepath.SkipDir
					}
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	out := make([]string, 0, len(matches))
	for path := range matches {
		out = append(out, path)
	}
	sort.Strings(out)
	return out, nil
}

type xcframeworkInfo struct {
	AvailableLibraries []struct {
		LibraryIdentifier        string   `plist:"LibraryIdentifier"`
		LibraryPath              string   `plist:"LibraryPath"`
		HeadersPath              string   `plist:"HeadersPath"`
		SupportedArchitectures   []string `plist:"SupportedArchitectures"`
		SupportedPlatform        string   `plist:"SupportedPlatform"`
		SupportedPlatformVariant string   `plist:"SupportedPlatformVariant"`
	} `plist:"AvailableLibraries"`
}

func selectXCFrameworkSlice(path string) (libraryPath, headersPath string, err error) {
	data, err := os.ReadFile(filepath.Join(path, "Info.plist"))
	if err != nil {
		return "", "", err
	}
	var info xcframeworkInfo
	if _, err := plist.Unmarshal(data, &info); err != nil {
		return "", "", fmt.Errorf("parse %s: %w", filepath.Join(path, "Info.plist"), err)
	}
	for _, library := range info.AvailableLibraries {
		if library.SupportedPlatform != "ios" || library.SupportedPlatformVariant != "" {
			continue
		}
		hasArm64 := false
		for _, arch := range library.SupportedArchitectures {
			if arch == "arm64" {
				hasArm64 = true
				break
			}
		}
		if !hasArm64 {
			continue
		}
		root := filepath.Join(path, library.LibraryIdentifier)
		selected := filepath.Join(root, library.LibraryPath)
		if !fileExists(selected) && !dirExists(selected) {
			continue
		}
		headers := ""
		if library.HeadersPath != "" {
			headers = filepath.Join(root, library.HeadersPath)
		}
		return selected, headers, nil
	}
	return "", "", fmt.Errorf("%s has no iOS arm64 device slice", path)
}

func frameworkIsDynamic(path string) (bool, error) {
	name := strings.TrimSuffix(filepath.Base(path), ".framework")
	binaryPath := filepath.Join(path, name)
	data, err := os.ReadFile(binaryPath)
	if err != nil {
		return false, err
	}
	if dynamic, ok, err := binaryImageIsDynamic(data); ok {
		return dynamic, err
	}
	return false, fmt.Errorf("cannot determine framework binary type for %s", binaryPath)
}

func binaryImageIsDynamic(data []byte) (dynamic, recognized bool, err error) {
	if len(data) >= 8 && string(data[:8]) == "!<arch>\n" {
		return false, true, nil
	}
	if f, openErr := macho.NewFile(bytes.NewReader(data)); openErr == nil {
		defer f.Close()
		return f.Type == macho.TypeDylib, true, nil
	}
	slice, universal, err := universalArm64Slice(data)
	if err != nil {
		return false, universal, err
	}
	if !universal {
		return false, false, nil
	}
	if result, ok, sliceErr := binaryImageIsDynamic(slice); ok {
		return result, true, sliceErr
	}
	return false, true, fmt.Errorf("unrecognized arm64 payload in Mach-O universal binary")
}

func universalArm64Slice(data []byte) ([]byte, bool, error) {
	if len(data) < 8 {
		return nil, false, nil
	}
	magic := binary.BigEndian.Uint32(data[:4])
	var order binary.ByteOrder
	var fat64 bool
	switch magic {
	case 0xcafebabe:
		order = binary.BigEndian
	case 0xbebafeca:
		order = binary.LittleEndian
	case 0xcafebabf:
		order = binary.BigEndian
		fat64 = true
	case 0xbfbafeca:
		order = binary.LittleEndian
		fat64 = true
	default:
		return nil, false, nil
	}

	count := int(order.Uint32(data[4:8]))
	entrySize := 20
	if fat64 {
		entrySize = 32
	}
	if count <= 0 || 8+count*entrySize > len(data) {
		return nil, true, fmt.Errorf("invalid Mach-O universal header")
	}
	for i := 0; i < count; i++ {
		base := 8 + i*entrySize
		cpu := order.Uint32(data[base : base+4])
		var offset, size uint64
		if fat64 {
			offset = order.Uint64(data[base+8 : base+16])
			size = order.Uint64(data[base+16 : base+24])
		} else {
			offset = uint64(order.Uint32(data[base+8 : base+12]))
			size = uint64(order.Uint32(data[base+12 : base+16]))
		}
		if offset > uint64(len(data)) || size > uint64(len(data))-offset {
			return nil, true, fmt.Errorf("invalid Mach-O universal slice bounds")
		}
		if cpu == uint32(macho.CpuArm64) {
			return data[offset : offset+size], true, nil
		}
	}
	return nil, true, fmt.Errorf("Mach-O universal binary has no arm64 slice")
}

func resolveVendoredArtifacts(base string, info podspecInfo) ([]vendoredFramework, []string, []string, error) {
	frameworkPaths, err := globPaths(base, info.VendoredFrameworks)
	if err != nil {
		return nil, nil, nil, err
	}
	libraryPaths, err := globPaths(base, info.VendoredLibraries)
	if err != nil {
		return nil, nil, nil, err
	}
	var frameworks []vendoredFramework
	var libraries []string
	var includeDirs []string
	for _, path := range frameworkPaths {
		if filepath.Ext(path) == ".xcframework" {
			selected, headers, err := selectXCFrameworkSlice(path)
			if err != nil {
				return nil, nil, nil, err
			}
			if headers != "" {
				includeDirs = append(includeDirs, headers)
			}
			path = selected
		}
		switch filepath.Ext(path) {
		case ".framework":
			dynamic, err := frameworkIsDynamic(path)
			if err != nil {
				return nil, nil, nil, err
			}
			frameworks = append(frameworks, vendoredFramework{
				Path: path, Name: strings.TrimSuffix(filepath.Base(path), ".framework"), Dynamic: dynamic,
			})
		case ".a":
			libraries = append(libraries, path)
		default:
			return nil, nil, nil, fmt.Errorf("unsupported vendored framework artifact %s", path)
		}
	}
	for _, path := range libraryPaths {
		if filepath.Ext(path) == ".xcframework" {
			selected, headers, err := selectXCFrameworkSlice(path)
			if err != nil {
				return nil, nil, nil, err
			}
			if headers != "" {
				includeDirs = append(includeDirs, headers)
			}
			path = selected
		}
		if filepath.Ext(path) != ".a" {
			return nil, nil, nil, fmt.Errorf("unsupported vendored library artifact %s", path)
		}
		libraries = append(libraries, path)
	}
	return frameworks, uniqueStrings(libraries), uniqueStrings(includeDirs), nil
}

type pluginSources struct {
	Swift              []string
	ObjC               []string
	CXX                []string
	Headers            []string
	IncludeDirs        []string
	Frameworks         []string
	Libraries          []string
	Resources          []string
	VendoredFrameworks []vendoredFramework
	VendoredLibraries  []string
	DeploymentTarget   string
	ModuleName         string
	SwiftVersion       string
}

type vendoredFramework struct {
	Path    string
	Name    string
	Dynamic bool
}

func resolvepluginSources(plugin Plugin) (pluginSources, error) {
	info := podspecInfo{Name: plugin.Name, ModuleName: plugin.Name, SwiftVersion: "5.0"}
	podspecs, err := filepath.Glob(filepath.Join(plugin.IOSDir, "*.podspec"))
	if err != nil {
		return pluginSources{}, err
	}
	if len(podspecs) > 0 {
		sort.Strings(podspecs)
		info, err = parsePodspec(podspecs[0])
		if err != nil {
			return pluginSources{}, err
		}
	}
	sources, err := globSources(plugin.IOSDir, info.SourceFiles)
	if err != nil {
		return pluginSources{}, err
	}
	if len(sources) == 0 {
		err := filepath.WalkDir(plugin.IOSDir, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() || strings.Contains(path, "Example") || filepath.Base(path) == "Package.swift" {
				return nil
			}
			ext := filepath.Ext(path)
			if ext == ".swift" || ext == ".m" {
				sources = append(sources, path)
			}
			return nil
		})
		if err != nil {
			return pluginSources{}, err
		}
		sort.Strings(sources)
	}
	if len(sources) == 0 {
		sources, err = globSources(plugin.IOSDir, []string{"Classes/**/*", "Sources/**/*"})
		if err != nil {
			return pluginSources{}, err
		}
	}
	vendoredFrameworks, vendoredLibraries, vendoredIncludes, err := resolveVendoredArtifacts(plugin.IOSDir, info)
	if err != nil {
		return pluginSources{}, fmt.Errorf("resolve vendored artifacts: %w", err)
	}
	for _, dependency := range info.Dependencies {
		if dependency != "Flutter" {
			return pluginSources{}, fmt.Errorf("plugin %s depends on CocoaPod %s; ripley refuses to silently omit pod dependencies", plugin.Name, dependency)
		}
	}
	result := pluginSources{
		Frameworks: info.Frameworks, Libraries: info.Libraries, Resources: info.Resources,
		VendoredFrameworks: vendoredFrameworks, VendoredLibraries: vendoredLibraries,
		DeploymentTarget: info.DeploymentTarget, ModuleName: info.ModuleName, SwiftVersion: info.SwiftVersion,
	}
	includeSet := make(map[string]bool)
	for _, dir := range vendoredIncludes {
		includeSet[dir] = true
	}
	for _, source := range sources {
		switch filepath.Ext(source) {
		case ".swift":
			result.Swift = append(result.Swift, source)
		case ".m", ".mm":
			result.ObjC = append(result.ObjC, source)
		case ".c", ".cc", ".cpp":
			result.CXX = append(result.CXX, source)
		case ".h":
			result.Headers = append(result.Headers, source)
			includeSet[filepath.Dir(source)] = true
		}
	}
	_ = filepath.WalkDir(plugin.IOSDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr == nil && d.IsDir() && d.Name() == "include" {
			includeSet[path] = true
		}
		return nil
	})
	for dir := range includeSet {
		result.IncludeDirs = append(result.IncludeDirs, dir)
	}
	sort.Strings(result.IncludeDirs)
	result.Frameworks = uniqueStrings(append([]string{"Flutter", "UIKit", "Foundation"}, result.Frameworks...))
	return result, nil
}

func swiftCompiler(tc toolchain.Toolchain) (string, error) {
	bin, err := tc.SwiftBin()
	if err != nil {
		return "", err
	}
	path := filepath.Join(bin, "swiftc")
	if !fileExists(path) {
		return "", fmt.Errorf("swiftc not found at %s", path)
	}
	return path, nil
}

func pluginClang(tc toolchain.Toolchain) (string, error) {
	bin, err := tc.SwiftBin()
	if err == nil {
		path := filepath.Join(bin, "clang")
		if fileExists(path) {
			return path, nil
		}
	}
	path, lookErr := exec.LookPath("clang")
	if lookErr != nil {
		if err != nil {
			return "", err
		}
		return "", errors.New("clang not found")
	}
	return path, nil
}

func iosResourceDir(tc toolchain.Toolchain) (string, error) {
	res := filepath.Join(tc.Root, "swift-resource-ios")
	marker := filepath.Join(res, ".done")
	swiftRoot, err := tc.SwiftToolchainRoot()
	if err != nil {
		return "", err
	}
	swiftLib, err := tc.SwiftLib()
	if err != nil {
		return "", err
	}
	sdk, err := tc.IOSSDK()
	if err != nil {
		return "", err
	}
	libs, err := tc.SwiftIOSLibs()
	if err != nil {
		return "", err
	}
	fingerprint := strings.Join([]string{swiftRoot, swiftLib, sdk, libs}, "\n") + "\n"
	if data, err := os.ReadFile(marker); err == nil && string(data) == fingerprint {
		return res, nil
	}
	if err := os.RemoveAll(res); err != nil {
		return "", err
	}
	if err := os.MkdirAll(res, 0o755); err != nil {
		return "", err
	}
	excluded := map[string]bool{
		"clang": true, "dispatch": true, "CoreFoundation": true, "Block": true, "os": true,
		"_FoundationCShims": true, "_foundation_unicode": true, "linux": true,
		"pm": true, "migrator": true, "host": true, "iphoneos": true,
		"iphonesimulator": true, "macosx": true, "appletvos": true, "watchos": true, "xros": true,
	}
	entries, err := os.ReadDir(swiftLib)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if excluded[entry.Name()] || strings.HasPrefix(entry.Name(), "layouts-") {
			continue
		}
		src := filepath.Join(swiftLib, entry.Name())
		dst := filepath.Join(res, entry.Name())
		if entry.IsDir() {
			if err := copyDir(src, dst); err != nil {
				return "", err
			}
		} else if err := copyFile(src, dst); err != nil {
			return "", err
		}
	}
	clangRoot := filepath.Join(swiftRoot, "usr", "lib", "clang")
	versions, err := os.ReadDir(clangRoot)
	if err != nil {
		return "", err
	}
	if len(versions) > 0 {
		latest := versions[0].Name()
		for _, version := range versions[1:] {
			if maxVersion(latest, version.Name()) == version.Name() {
				latest = version.Name()
			}
		}
		target := filepath.Join(clangRoot, latest)
		if err := os.Symlink(target, filepath.Join(res, "clang")); err != nil {
			return "", err
		}
	}
	if dirExists(libs) {
		if err := os.Symlink(libs, filepath.Join(res, "iphoneos")); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(marker, []byte(fingerprint), 0o644); err != nil {
		return "", err
	}
	return res, nil
}

func commonSwiftFlags(tc toolchain.Toolchain, minOS string) ([]string, error) {
	sdk, err := tc.IOSSDK()
	if err != nil {
		return nil, err
	}
	resourceDir, err := iosResourceDir(tc)
	if err != nil {
		return nil, err
	}
	flags := []string{"-target", "arm64-apple-ios" + minOS, "-sdk", sdk, "-resource-dir", resourceDir}
	prebuiltFlags, err := swiftPrebuiltModuleFlags(tc)
	if err != nil {
		return nil, err
	}
	flags = append(flags, prebuiltFlags...)
	flags = append(flags, swiftShimImportFlags(resourceDir, sdk)...)
	flags = append(flags, "-parse-as-library", "-emit-object", "-O", "-whole-module-optimization")
	return flags, nil
}

// Xcode 27 moved the canonical serialized iPhoneOS Swift modules out of the
// SDK and into XcodeDefault.xctoolchain/usr/lib/swift/iphoneos/prebuilt-modules.
// Prefer the directory matching the SDK version from the same Swift runtime
// payload. This keeps Linux Swift from rebuilding Apple's arm64e textual
// interfaces, which can fail even when the open-source compiler has the same
// Swift language version.
func swiftPrebuiltModulesDir(tc toolchain.Toolchain) (string, error) {
	libs, err := tc.SwiftIOSLibs()
	if err != nil {
		return "", err
	}
	prebuiltRoot := filepath.Join(libs, "prebuilt-modules")
	sdkVersion, versionErr := tc.IOSSDKVersion()
	if !dirExists(prebuiltRoot) {
		// Unit fixtures and older SDK payloads may not carry SDKSettings.json;
		// preserve the legacy fallback in that case. A real Xcode 27+ SDK does,
		// and its textual Swift interfaces are not a safe fallback on Linux.
		if versionErr == nil && maxVersion(sdkVersion, "27.0") == sdkVersion {
			return "", fmt.Errorf("iPhoneOS SDK %s requires Apple Swift prebuilt modules; expected %s from the matching XcodeDefault.xctoolchain/usr/lib/swift/iphoneos payload (or set RIPLEY_SWIFT_IOS_LIBS to that directory)", sdkVersion, prebuiltRoot)
		}
		return "", nil
	}
	if versionErr != nil {
		return "", versionErr
	}
	dir := filepath.Join(prebuiltRoot, sdkVersion)
	if dirExists(dir) {
		return dir, nil
	}
	if maxVersion(sdkVersion, "27.0") == sdkVersion {
		return "", fmt.Errorf("iPhoneOS SDK %s requires matching Apple Swift prebuilt modules at %s", sdkVersion, dir)
	}
	return "", nil
}

func swiftPrebuiltModuleFlags(tc toolchain.Toolchain) ([]string, error) {
	dir, err := swiftPrebuiltModulesDir(tc)
	if err != nil || dir == "" {
		return nil, err
	}
	return []string{"-I", dir}, nil
}

// Swift 6.4 no longer makes the shims module discoverable merely because a
// custom -resource-dir is supplied. The hybrid iOS resource tree that ripley
// stages contains the Linux compiler's target-neutral shims, so expose that
// module explicitly to ClangImporter. This is harmless on older Swift versions
// and keeps the resource tree self-contained for both x86_64 and arm64 hosts.
func swiftShimImportFlags(resourceDir, sdk string) []string {
	shims := filepath.Join(resourceDir, "shims")
	moduleMap := filepath.Join(shims, "module.modulemap")
	if !fileExists(moduleMap) {
		return nil
	}
	flags := []string{"-Xcc", "-I", "-Xcc", shims}
	// Swift 6.4's SwiftShims imports C++ standard headers (for example
	// <type_traits>). Linux Clang does not infer Apple's libc++ include directory
	// when targeting Darwin, even though it is present in the iPhoneOS SDK.
	// Expose it explicitly to ClangImporter before loading the shims module.
	libcxx := filepath.Join(sdk, "usr", "include", "c++", "v1")
	if dirExists(libcxx) {
		flags = append(flags, "-Xcc", "-I", "-Xcc", libcxx)
	}
	return append(flags, "-Xcc", "-fmodule-map-file="+moduleMap)
}

func frameworkSearch(tc toolchain.Toolchain) ([]string, error) {
	sdk, err := tc.IOSSDK()
	if err != nil {
		return nil, err
	}
	out := []string{"-F", filepath.Join(tc.FlutterXCFramework(), "ios-arm64")}
	for _, dir := range []string{
		filepath.Join(sdk, "System", "Library", "Frameworks"),
		filepath.Join(sdk, "System", "Library", "SubFrameworks"),
		filepath.Join(sdk, "System", "Library", "PrivateFrameworks"),
	} {
		if dirExists(dir) {
			out = append(out, "-F", dir)
		}
	}
	return out, nil
}

func libSearch(tc toolchain.Toolchain) ([]string, error) {
	sdk, err := tc.IOSSDK()
	if err != nil {
		return nil, err
	}
	libs, err := tc.SwiftIOSLibs()
	if err != nil {
		return nil, err
	}
	return []string{"-L", filepath.Join(sdk, "usr", "lib"), "-L", filepath.Join(sdk, "usr", "lib", "swift"), "-L", libs}, nil
}

type pluginCompileResult struct {
	Framework          string
	EmbeddedFrameworks []string
}

func compilePlugin(tc toolchain.Toolchain, plugin Plugin, work, appMinOS string) (pluginCompileResult, error) {
	sources, err := resolvepluginSources(plugin)
	if err != nil {
		return pluginCompileResult{}, err
	}
	result := pluginCompileResult{}
	for _, framework := range sources.VendoredFrameworks {
		if framework.Dynamic {
			result.EmbeddedFrameworks = append(result.EmbeddedFrameworks, framework.Path)
		}
	}
	if len(sources.Swift) == 0 && len(sources.ObjC) == 0 && len(sources.CXX) == 0 {
		return result, nil
	}
	pluginDir := filepath.Join(work, plugin.Name)
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return pluginCompileResult{}, err
	}
	minOS := maxVersion(sources.DeploymentTarget, appMinOS)
	sources.DeploymentTarget = minOS
	var objects []string
	search, err := frameworkSearch(tc)
	if err != nil {
		return pluginCompileResult{}, err
	}
	for _, framework := range sources.VendoredFrameworks {
		search = append(search, "-F", filepath.Dir(framework.Path))
	}
	if len(sources.Swift) > 0 {
		swiftc, err := swiftCompiler(tc)
		if err != nil {
			return pluginCompileResult{}, err
		}
		flags, err := commonSwiftFlags(tc, minOS)
		if err != nil {
			return pluginCompileResult{}, err
		}
		obj := filepath.Join(pluginDir, plugin.Name+"-swift.o")
		header := filepath.Join(pluginDir, sources.ModuleName+"-Swift.h")
		args := append(flags, search...)
		if sources.SwiftVersion != "" {
			args = append(args, "-swift-version", swiftLanguageVersion(sources.SwiftVersion))
		}
		args = append(args, sources.Swift...)
		args = append(args, "-module-name", sources.ModuleName, "-emit-objc-header-path", header, "-o", obj)
		for _, dir := range sources.IncludeDirs {
			args = append(args, "-Xcc", "-I", "-Xcc", dir)
		}
		if err := run("", nil, swiftc, args...); err != nil {
			return pluginCompileResult{}, err
		}
		objects = append(objects, obj)
		sources.Headers = append(sources.Headers, header)
	}
	clang, err := pluginClang(tc)
	if err != nil {
		return pluginCompileResult{}, err
	}
	sdk, err := tc.IOSSDK()
	if err != nil {
		return pluginCompileResult{}, err
	}
	for _, source := range append(append([]string{}, sources.ObjC...), sources.CXX...) {
		obj := filepath.Join(pluginDir, strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))+".o")
		args := []string{"-target", "arm64-apple-ios" + minOS, "-isysroot", sdk, "-fobjc-arc", "-fmodules", "-O2", "-c", source, "-o", obj}
		args = append(args, search...)
		for _, dir := range sources.IncludeDirs {
			args = append(args, "-I", dir)
		}
		if err := run("", nil, clang, args...); err != nil {
			return pluginCompileResult{}, err
		}
		objects = append(objects, obj)
	}

	framework := filepath.Join(work, plugin.Name+".framework")
	if err := os.MkdirAll(framework, 0o755); err != nil {
		return pluginCompileResult{}, err
	}
	binary := filepath.Join(framework, plugin.Name)
	sdkVersion, err := tc.IOSSDKVersion()
	if err != nil {
		return pluginCompileResult{}, err
	}
	args := []string{"-arch", "arm64", "-dylib", "-platform_version", "ios", minOS, sdkVersion, "-syslibroot", sdk, "-install_name", "@rpath/" + plugin.Name + ".framework/" + plugin.Name, "-o", binary}
	args = append(args, objects...)
	for _, frameworkName := range sources.Frameworks {
		args = append(args, "-framework", frameworkName)
	}
	for _, framework := range sources.VendoredFrameworks {
		args = append(args, "-framework", framework.Name, "-F", filepath.Dir(framework.Path))
	}
	for _, library := range sources.Libraries {
		args = append(args, "-l"+strings.TrimPrefix(library, "lib"))
	}
	args = append(args, sources.VendoredLibraries...)
	if len(sources.Swift) > 0 {
		args = append(args, "-lswiftCore", "-lswiftFoundation", "-lswiftDarwin", "-lswiftObjectiveC", "-lswiftDispatch", "-lswiftCoreFoundation", "-lswiftCompatibility56", "-lswiftCompatibilityPacks", "-lswiftCompatibilityConcurrency", "-rpath", "/usr/lib/swift")
	}
	libs, err := libSearch(tc)
	if err != nil {
		return pluginCompileResult{}, err
	}
	args = append(args, search...)
	args = append(args, libs...)
	swiftIOSLibs, err := tc.SwiftIOSLibs()
	if err != nil {
		return pluginCompileResult{}, err
	}
	runtimeLib := filepath.Join(swiftIOSLibs, "libclang_rt.ios.a")
	if fileExists(runtimeLib) {
		args = append(args, runtimeLib)
	}
	args = append(args, "-lSystem")
	if err := runLD64(tc, args...); err != nil {
		return pluginCompileResult{}, err
	}
	if err := os.Chmod(binary, 0o755); err != nil {
		return pluginCompileResult{}, err
	}
	if err := copyPluginResources(plugin, sources, framework); err != nil {
		return pluginCompileResult{}, err
	}
	if err := writeFrameworkSkeleton(framework, plugin.Name, sources); err != nil {
		return pluginCompileResult{}, err
	}
	result.Framework = framework
	return result, nil
}

func copyPluginResources(plugin Plugin, sources pluginSources, framework string) error {
	podspecs, _ := filepath.Glob(filepath.Join(plugin.IOSDir, "*.podspec"))
	if len(podspecs) == 0 {
		return nil
	}
	data, err := os.ReadFile(podspecs[0])
	if err != nil {
		return err
	}
	text := string(data)
	re := regexp.MustCompile(`s\.resource_bundles\s*=\s*\{[^}]*?['"]([^'"]+)['"]\s*=>\s*\[([^\]]*)\]`)
	for _, match := range re.FindAllStringSubmatch(text, -1) {
		bundleDir := filepath.Join(framework, match[1]+".bundle")
		for _, quoted := range quotedStringRE.FindAllStringSubmatch(match[2], -1) {
			files, err := globResourceFiles(plugin.IOSDir, quoted[1])
			if err != nil {
				return err
			}
			for _, file := range files {
				if err := os.MkdirAll(bundleDir, 0o755); err != nil {
					return err
				}
				if err := copyFile(file, filepath.Join(bundleDir, filepath.Base(file))); err != nil {
					return err
				}
			}
		}
	}
	for _, pattern := range sources.Resources {
		files, err := globResourceFiles(plugin.IOSDir, pattern)
		if err != nil {
			return err
		}
		for _, file := range files {
			if err := copyFile(file, filepath.Join(framework, filepath.Base(file))); err != nil {
				return err
			}
		}
	}
	return nil
}

func globResourceFiles(base, pattern string) ([]string, error) {
	re, err := globPatternRegexp(filepath.ToSlash(pattern))
	if err != nil {
		return nil, err
	}
	var files []string
	err = filepath.WalkDir(base, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		if re.MatchString(filepath.ToSlash(rel)) {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

type pluginFrameworkInfo struct {
	BundleIdentifier  string `plist:"CFBundleIdentifier"`
	BundleName        string `plist:"CFBundleName"`
	BundleExecutable  string `plist:"CFBundleExecutable"`
	BundlePackageType string `plist:"CFBundlePackageType"`
	ShortVersion      string `plist:"CFBundleShortVersionString"`
	BundleVersion     string `plist:"CFBundleVersion"`
	MinimumOSVersion  string `plist:"MinimumOSVersion"`
}

func writeFrameworkSkeleton(framework, name string, sources pluginSources) error {
	headers := filepath.Join(framework, "Headers")
	modules := filepath.Join(framework, "Modules")
	if err := os.MkdirAll(headers, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(modules, 0o755); err != nil {
		return err
	}
	byName := make(map[string]string)
	for _, header := range sources.Headers {
		byName[filepath.Base(header)] = header
	}
	for headerName, source := range byName {
		if err := replaceFile(source, filepath.Join(headers, headerName)); err != nil {
			return err
		}
	}
	var lines []string
	lines = append(lines, "framework module "+name+" {")
	if fileExists(filepath.Join(headers, name+".h")) {
		lines = append(lines, "  umbrella header \""+name+".h\"")
	} else {
		names := make([]string, 0, len(byName))
		for headerName := range byName {
			names = append(names, headerName)
		}
		sort.Strings(names)
		for _, headerName := range names {
			lines = append(lines, "  header \""+headerName+"\"")
		}
		if len(sources.Swift) > 0 {
			lines = append(lines, "  header \""+name+"-Swift.h\"")
		}
	}
	lines = append(lines, "  export *", "}")
	if err := os.WriteFile(filepath.Join(modules, "module.modulemap"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return writePlist(filepath.Join(framework, "Info.plist"), pluginFrameworkInfo{
		BundleIdentifier:  "org.flutter." + name,
		BundleName:        name,
		BundleExecutable:  name,
		BundlePackageType: "FMWK",
		ShortVersion:      "1.0",
		BundleVersion:     "1",
		MinimumOSVersion:  sources.DeploymentTarget,
	})
}

const registrantHeader = `#ifndef GeneratedPluginRegistrant_h
#define GeneratedPluginRegistrant_h
#import <Flutter/Flutter.h>
@interface GeneratedPluginRegistrant : NSObject
+ (void)registerWithRegistry:(NSObject<FlutterPluginRegistry>*)registry;
@end
#endif
`

func registrantSource(plugins []Plugin) string {
	lines := []string{"#import \"GeneratedPluginRegistrant.h\""}
	for _, plugin := range plugins {
		class := plugin.PluginClass
		if class == "" {
			class = defaultPluginClass(plugin.Name)
		}
		lines = append(lines, fmt.Sprintf("#if __has_include(<%s/%s.h>)\n#import <%s/%s.h>\n#else\n@import %s;\n#endif\n", plugin.Name, class, plugin.Name, class, plugin.Name))
	}
	lines = append(lines, "@implementation GeneratedPluginRegistrant\n+ (void)registerWithRegistry:(NSObject<FlutterPluginRegistry>*)registry {")
	for _, plugin := range plugins {
		class := plugin.PluginClass
		if class == "" {
			class = defaultPluginClass(plugin.Name)
		}
		lines = append(lines, fmt.Sprintf("  [%s registerWithRegistrar:[registry registrarForPlugin:@\"%s\"]];", class, class))
	}
	lines = append(lines, "}\n@end\n")
	return strings.Join(lines, "\n")
}

func defaultPluginClass(name string) string {
	parts := strings.Split(name, "_")
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + strings.ToLower(part[1:])
	}
	return strings.Join(parts, "") + "Plugin"
}

func BuildRegistrant(tc toolchain.Toolchain, plugins []Plugin, work, sourceDir, minOS string, extraIncludeDirs, extraFrameworkDirs []string) (string, error) {
	dir := filepath.Join(work, "registrant")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	source := filepath.Join(sourceDir, "GeneratedPluginRegistrant.m")
	header := filepath.Join(sourceDir, "GeneratedPluginRegistrant.h")
	includeDir := sourceDir
	if !fileExists(source) || !fileExists(header) {
		includeDir = dir
		header = filepath.Join(dir, "GeneratedPluginRegistrant.h")
		if err := os.WriteFile(header, []byte(registrantHeader), 0o644); err != nil {
			return "", err
		}
		source = filepath.Join(dir, "GeneratedPluginRegistrant.m")
		if err := os.WriteFile(source, []byte(registrantSource(plugins)), 0o644); err != nil {
			return "", err
		}
	}
	object := filepath.Join(dir, "GeneratedPluginRegistrant.o")
	clang, err := pluginClang(tc)
	if err != nil {
		return "", err
	}
	sdk, err := tc.IOSSDK()
	if err != nil {
		return "", err
	}
	search, err := frameworkSearch(tc)
	if err != nil {
		return "", err
	}
	headerPaths := append([]string{includeDir}, extraIncludeDirs...)
	args := []string{"-target", "arm64-apple-ios" + minOS, "-isysroot", sdk, "-fobjc-arc", "-fmodules", "-O2", "-c", source, "-o", object}
	for _, dir := range headerPaths {
		if dir != "" {
			args = append(args, "-I", dir)
		}
	}
	for _, moduleMap := range clangModuleMaps(headerPaths, nil) {
		args = append(args, "-fmodule-map-file="+moduleMap)
	}
	for _, dir := range extraFrameworkDirs {
		if dir != "" {
			args = append(args, "-F", dir)
		}
	}
	args = append(args, search...)
	frameworks, _ := filepath.Glob(filepath.Join(work, "*.framework"))
	for _, framework := range frameworks {
		args = append(args, "-F", filepath.Dir(framework))
	}
	if err := run("", nil, clang, args...); err != nil {
		return "", err
	}
	return object, nil
}

func BuildAllPlugins(tc toolchain.Toolchain, projectRoot, sourceDir, minOS, swiftVersion, packageConfigPath string) (PluginBuild, error) {
	plugins, err := DiscoverPlugins(projectRoot, packageConfigPath)
	if err != nil {
		return PluginBuild{}, err
	}
	var native []Plugin
	for _, plugin := range plugins {
		if plugin.NativeBuild {
			native = append(native, plugin)
		}
	}
	if len(native) == 0 {
		return PluginBuild{}, nil
	}
	needsPods, err := pluginsNeedCocoaPods(native)
	if err != nil {
		return PluginBuild{}, err
	}
	if needsPods {
		return buildCocoaPods(tc, projectRoot, sourceDir, native, minOS, swiftVersion)
	}
	work := filepath.Join(projectRoot, "build", "ripley_ios", "plugins")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return PluginBuild{}, err
	}
	result := PluginBuild{Plugins: native}
	for _, plugin := range native {
		built, err := compilePlugin(tc, plugin, work, minOS)
		if err != nil {
			return PluginBuild{}, fmt.Errorf("plugin %s failed to compile: %w", plugin.Name, err)
		}
		if built.Framework != "" {
			result.Frameworks = append(result.Frameworks, built.Framework)
			result.EmbeddedFrameworks = append(result.EmbeddedFrameworks, built.Framework)
			fmt.Printf("  built %s\n", filepath.Base(built.Framework))
		}
		for _, framework := range built.EmbeddedFrameworks {
			if !containsString(result.Frameworks, framework) {
				result.Frameworks = append(result.Frameworks, framework)
				fmt.Printf("  vendored %s\n", filepath.Base(framework))
			}
			if !containsString(result.EmbeddedFrameworks, framework) {
				result.EmbeddedFrameworks = append(result.EmbeddedFrameworks, framework)
			}
		}
	}
	result.RegistrantObject, err = BuildRegistrant(tc, native, work, sourceDir, minOS, nil, nil)
	if err != nil {
		return PluginBuild{}, err
	}
	return result, nil
}

func swiftLanguageVersion(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "6.") {
		return "6"
	}
	if strings.HasPrefix(value, "5.") {
		return "5"
	}
	if value == "4.0" {
		return "4"
	}
	return value
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}
