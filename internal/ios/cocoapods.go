package ios

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"ripley/internal/toolchain"
)

type podSource struct {
	Path  string   `json:"path"`
	Flags []string `json:"flags"`
}

type podHeader struct {
	Path       string   `json:"path"`
	Attributes []string `json:"attributes"`
}

type podShellScriptPhase struct {
	Name                               string   `json:"name"`
	ShellPath                          string   `json:"shell_path"`
	Script                             string   `json:"script"`
	InputPaths                         []string `json:"input_paths"`
	OutputPaths                        []string `json:"output_paths"`
	InputFileListPaths                 []string `json:"input_file_list_paths"`
	OutputFileListPaths                []string `json:"output_file_list_paths"`
	DependencyFile                     string   `json:"dependency_file"`
	Order                              int      `json:"order"`
	BeforeSources                      bool     `json:"before_sources"`
	BeforeHeaders                      bool     `json:"before_headers"`
	AlwaysOutOfDate                    bool     `json:"always_out_of_date"`
	RunOnlyForDeploymentPostprocessing bool     `json:"run_only_for_deployment_postprocessing"`
}

type podTarget struct {
	Name                     string                `json:"name"`
	ProductType              string                `json:"product_type"`
	ProductName              string                `json:"product_name"`
	ModuleName               string                `json:"module_name"`
	SwiftVersion             string                `json:"swift_version"`
	DeploymentTarget         string                `json:"deployment_target"`
	ArcEnabled               bool                  `json:"arc_enabled"`
	PrefixHeader             string                `json:"prefix_header"`
	Dependencies             []string              `json:"dependencies"`
	Sources                  []podSource           `json:"sources"`
	Headers                  []podHeader           `json:"headers"`
	Resources                []string              `json:"resources"`
	HeaderSearchPaths        []string              `json:"header_search_paths"`
	FrameworkSearchPaths     []string              `json:"framework_search_paths"`
	LibrarySearchPaths       []string              `json:"library_search_paths"`
	SwiftIncludePaths        []string              `json:"swift_include_paths"`
	PreprocessorDefinitions  []string              `json:"preprocessor_definitions"`
	OtherCFlags              []string              `json:"other_cflags"`
	OtherCXXFlags            []string              `json:"other_cxxflags"`
	OtherSwiftFlags          []string              `json:"other_swift_flags"`
	OtherLDFlags             []string              `json:"other_ldflags"`
	BaseOtherLDFlags         []string              `json:"base_other_ldflags"`
	BaseFrameworkSearchPaths []string              `json:"base_framework_search_paths"`
	BaseLibrarySearchPaths   []string              `json:"base_library_search_paths"`
	BuildSettings            map[string]string     `json:"build_settings"`
	ShellScriptPhases        []podShellScriptPhase `json:"shell_script_phases"`
}

type podManifest struct {
	PodsRoot         string      `json:"pods_root"`
	ProjectRoot      string      `json:"-"` // Flutter package root; user script phases may need package metadata from their working directory.
	Targets          []podTarget `json:"targets"`
	FrameworkLinkage string      `json:"-"` // "dynamic" for bare/dynamic use_frameworks!, "static" for explicit static linkage
}

func podspecForPlugin(plugin Plugin) (string, podspecInfo, error) {
	podspecs, err := filepath.Glob(filepath.Join(plugin.IOSDir, "*.podspec"))
	if err != nil {
		return "", podspecInfo{}, err
	}
	if len(podspecs) == 0 {
		return "", podspecInfo{}, nil
	}
	sort.Strings(podspecs)
	info, err := parsePodspec(podspecs[0])
	return podspecs[0], info, err
}

func pluginsNeedCocoaPods(plugins []Plugin) (bool, error) {
	for _, plugin := range plugins {
		_, info, err := podspecForPlugin(plugin)
		if err != nil {
			return false, err
		}
		for _, dependency := range info.Dependencies {
			if dependency != "Flutter" {
				return true, nil
			}
		}
	}
	return false, nil
}

func cocoaPodsExecutable() (string, error) {
	if path, err := exec.LookPath("pod"); err == nil {
		return path, nil
	}
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		return "", errors.New("CocoaPods dependency resolution requires ruby and CocoaPods 1.16+ on the Linux build host")
	}
	cmd := exec.Command(ruby, "-e", `print Gem.user_dir`)
	out, err := cmd.Output()
	if err == nil {
		candidate := filepath.Join(strings.TrimSpace(string(out)), "bin", "pod")
		if fileExists(candidate) {
			return candidate, nil
		}
	}
	return "", errors.New("CocoaPods is required for plugins with external pod dependencies; install with: gem install --user-install cocoapods -v 1.16.2 --no-document")
}

func rubyQuote(value string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), "'", `\'`) + "'"
}

// writeResolverPodfile synthesizes the Podfile used only to resolve the pod
// graph. Besides the Flutter stub and every Flutter plugin, it re-emits the
// explicit `pod '<name>', :path => '<dir>'` declarations of the project's own
// ios/Podfile: plugins may depend on a pod that ships as a subdirectory of
// another plugin and exists on no CDN (share_handler_ios depends on
// share_handler_ios_models, which lives in share_handler_ios/ios/Models), so
// omitting them leaves the graph unsatisfiable.
func writeResolverPodfile(path, projectRoot string, plugins []Plugin, minOS, swiftVersion string) error {
	resolverRoot := filepath.Dir(path)
	flutterDir := filepath.Join(resolverRoot, "Flutter")
	if err := os.MkdirAll(flutterDir, 0o755); err != nil {
		return err
	}
	flutterSpec := `Pod::Spec.new do |s|
  s.name = 'Flutter'
  s.version = '1.0.0'
  s.summary = 'Resolver-only Flutter stub for ripley Linux builds'
  s.homepage = 'https://flutter.dev'
  s.license = { :type => 'BSD' }
  s.author = 'Flutter'
  s.source = { :path => '.' }
  s.source_files = 'empty.m'
  s.platform = :ios, '12.0'
end
`
	if err := os.WriteFile(filepath.Join(flutterDir, "Flutter.podspec"), []byte(flutterSpec), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(flutterDir, "empty.m"), []byte("// resolver stub\n"), 0o644); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintln(&b, `source 'https://cdn.cocoapods.org/'`)
	fmt.Fprintf(&b, "platform :ios, %s\n", rubyQuote(minOS))
	fmt.Fprintln(&b, `install! 'cocoapods', :integrate_targets => false`)
	fmt.Fprintln(&b, `use_modular_headers!`)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, `target 'FLResolver' do`)
	// With integration disabled CocoaPods has no user PBXNativeTarget from which
	// to infer SWIFT_VERSION for legacy Swift pods. Preserve the selected app
	// target's effective setting directly on the synthetic target definition.
	// This is not a Swift-version constraint: pods that declare newer supported
	// versions (for example Sentry 5.5) still select their own compatible version.
	if strings.TrimSpace(swiftVersion) != "" {
		fmt.Fprintf(&b, "  current_target_definition.swift_version = %s\n", rubyQuote(strings.TrimSpace(swiftVersion)))
	}
	frameworks, linkage, err := projectPodFrameworkLinkage(projectRoot)
	if err != nil {
		return err
	}
	if frameworks {
		if linkage == "" {
			fmt.Fprintln(&b, `  use_frameworks!`)
		} else {
			fmt.Fprintf(&b, "  use_frameworks! :linkage => :%s\n", linkage)
		}
	}
	fmt.Fprintf(&b, "  pod 'Flutter', :path => %s\n", rubyQuote(flutterDir))
	pluginLinks := filepath.Join(resolverRoot, "plugins")
	if err := os.MkdirAll(pluginLinks, 0o755); err != nil {
		return err
	}
	for _, plugin := range plugins {
		podspec, info, err := podspecForPlugin(plugin)
		if err != nil {
			return err
		}
		if podspec == "" {
			continue
		}
		name := info.Name
		if name == "" {
			name = plugin.Name
		}
		link := filepath.Join(pluginLinks, plugin.Name)
		_ = os.RemoveAll(link)
		if err := os.Symlink(plugin.Path, link); err != nil {
			return fmt.Errorf("create Flutter-style plugin symlink for %s: %w", plugin.Name, err)
		}
		relIOS, err := filepath.Rel(plugin.Path, plugin.IOSDir)
		if err != nil {
			return err
		}
		podPath := filepath.Join(link, relIOS)
		fmt.Fprintf(&b, "  pod %s, :path => %s\n", rubyQuote(name), rubyQuote(podPath))
	}
	emitted := make(map[string]bool, len(plugins))
	for _, plugin := range plugins {
		emitted[plugin.Name] = true
	}
	extra, err := projectPathPods(projectRoot, plugins)
	if err != nil {
		return err
	}
	for _, pod := range extra {
		if emitted[pod.Name] {
			continue
		}
		emitted[pod.Name] = true
		fmt.Fprintf(&b, "  pod %s, :path => %s\n", rubyQuote(pod.Name), rubyQuote(pod.Path))
	}
	fmt.Fprintln(&b, `end`)
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// pathPod is an explicit local pod declaration read from a project Podfile.
type pathPod struct {
	Name string
	Path string
}

var podfilePathPodRE = regexp.MustCompile(`(?m)^\s*pod\s+['"]([^'"]+)['"]\s*,\s*:path\s*=>\s*['"]([^'"]+)['"]`)
var podfileUseFrameworksRE = regexp.MustCompile(`(?m)^\s*use_frameworks!\s*(?::linkage\s*=>\s*:(static|dynamic))?`)

// projectPodFrameworkLinkage preserves the product/linkage model selected by
// the application's Podfile. CocoaPods defaults a bare use_frameworks! to
// dynamic frameworks; an explicit :linkage option is kept verbatim so the
// resolver emits the same pod target product types as the real integration.
func projectPodFrameworkLinkage(projectRoot string) (bool, string, error) {
	if projectRoot == "" {
		return false, "", nil
	}
	data, err := os.ReadFile(filepath.Join(projectRoot, "ios", "Podfile"))
	if errors.Is(err, os.ErrNotExist) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	match := podfileUseFrameworksRE.FindSubmatch(data)
	if match == nil {
		return false, "", nil
	}
	linkage := ""
	if len(match) > 1 {
		linkage = string(match[1])
	}
	return true, linkage, nil
}

// projectPathPods reads the project's ios/Podfile and returns its explicit
// local pod declarations, from every target including nested ones. Literal
// paths are resolved against the Podfile's directory. Flutter's generated
// `.symlinks/plugins/<plugin>/...` paths are resolved directly through the
// discovered plugin root when the generated symlink tree does not exist yet;
// this is the normal pre-pod-install state on a clean checkout.
func projectPathPods(projectRoot string, plugins []Plugin) ([]pathPod, error) {
	if projectRoot == "" {
		return nil, nil
	}
	iosDir := filepath.Join(projectRoot, "ios")
	data, err := os.ReadFile(filepath.Join(iosDir, "Podfile"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pluginRoots := make(map[string]string, len(plugins))
	for _, plugin := range plugins {
		if plugin.Name != "" && plugin.Path != "" {
			pluginRoots[plugin.Name] = plugin.Path
		}
	}
	resolvePath := func(raw string) string {
		dir := raw
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(iosDir, dir)
		}
		if dirExists(dir) || fileExists(dir) {
			return filepath.Clean(dir)
		}
		// flutter_install_all_ios_pods creates ios/.symlinks/plugins lazily as
		// part of the normal CocoaPods path. ripley resolves the graph before that
		// step, so map the same logical path through DiscoverPlugins instead.
		rel := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(raw)), "./")
		const prefix = ".symlinks/plugins/"
		if !strings.HasPrefix(rel, prefix) {
			return ""
		}
		rest := strings.TrimPrefix(rel, prefix)
		parts := strings.SplitN(rest, "/", 2)
		root := pluginRoots[parts[0]]
		if root == "" {
			return ""
		}
		dir = root
		if len(parts) == 2 && parts[1] != "" {
			dir = filepath.Join(root, filepath.FromSlash(parts[1]))
		}
		if dirExists(dir) || fileExists(dir) {
			return filepath.Clean(dir)
		}
		return ""
	}

	var pods []pathPod
	for _, match := range podfilePathPodRE.FindAllStringSubmatch(string(data), -1) {
		dir := resolvePath(match[2])
		// The declaration may name either the podspec's directory or the
		// podspec itself; CocoaPods accepts both. Missing paths are ignored here
		// because the declaration may be configuration-specific.
		if dir == "" {
			continue
		}
		pods = append(pods, pathPod{Name: match[1], Path: dir})
	}
	return pods, nil
}

const podManifestRuby = `
require 'json'
require 'shellwords'
require 'xcodeproj'

def expand_value(value, vars)
  return '' if value.nil?
  result = value.is_a?(Array) ? value.join(' ') : value.to_s
  16.times do
    before = result
    result = result.gsub(/\$\(([^)]+)\)|\$\{([^}]+)\}/) do |match|
      raw = $1 || $2
      key = raw.split(':', 2).first
      if key == 'inherited'
        ''
      elsif vars.key?(key)
        vars[key].to_s
      else
        match
      end
    end
    break if result == before
  end
  result
end

def list_value(value, vars)
  text = expand_value(value, vars).gsub('$(inherited)', '').gsub('${inherited}', '')
  Shellwords.shellsplit(text)
rescue ArgumentError
  text.split(/\s+/).reject(&:empty?)
end

# The extractor has a fixed build context (Release, arm64, iphoneos). Xcode
# build settings may add device-only values with condition suffixes such as
# OTHER_LDFLAGS[sdk=iphoneos*]. Keep the unconditional CocoaPods-generated
# values and append every matching conditional variant; simulator-only values
# must never leak into a device link.
def condition_matches?(key, context)
  conditions = key.scan(/\[([^=\]]+)=([^\]]+)\]/)
  return false if conditions.empty?
  conditions.all? do |name, pattern|
    actual = context[name]
    !actual.nil? && File.fnmatch(pattern, actual)
  end
end

def effective_list(raw, name, vars)
  context = {
    'sdk' => 'iphoneos',
    'arch' => 'arm64',
    'config' => 'Release',
    'variant' => 'normal',
    'platform' => 'iphoneos',
  }
  values = list_value(raw[name], vars)
  raw.keys.map(&:to_s).sort.each do |key|
    next unless key.start_with?(name + '[')
    next unless condition_matches?(key, context)
    source_key = raw.key?(key) ? key : raw.keys.find { |candidate| candidate.to_s == key }
    values.concat(list_value(raw[source_key], vars)) if source_key
  end
  values
end

# CocoaPods sometimes places a PBXAggregateTarget between a pod target and the
# actual static-library targets it consumes (for example firebase_messaging ->
# Firebase -> FirebaseCore + FirebaseMessaging). Aggregate targets produce no
# linkable artifact, so normalize them out here and expose only concrete native
# dependencies to the Go build graph.
def native_dependency_names(target, seen = {})
  target.dependencies.flat_map do |dependency|
    child = dependency.target
    next [] unless child
    if child.respond_to?(:product_type)
      [child.name]
    else
      key = child.uuid.to_s
      next [] if seen[key]
      nested_seen = seen.merge(key => true)
      native_dependency_names(child, nested_seen)
    end
  end.uniq
end

project_path = ARGV.fetch(0)
project = Xcodeproj::Project.open(project_path)
pods_root = File.dirname(project_path)
build_dir = ENV.fetch('RIPLEY_POD_BUILD_DIR')
toolchain_dir = ENV.fetch('RIPLEY_TOOLCHAIN_DIR', '')
ios_sdk = ENV.fetch('RIPLEY_IOS_SDK', '')

targets = project.targets.map do |target|
  next unless target.respond_to?(:product_type)
  config = target.build_configurations.find { |c| c.name == 'Release' } || target.build_configurations.first
  xc = config.base_configuration_reference && File.exist?(config.base_configuration_reference.real_path.to_s) ? Xcodeproj::Config.new(config.base_configuration_reference.real_path.to_s).to_hash : {}
  raw = xc.merge(config.build_settings)
  target_build_dir = File.join(build_dir, 'Release-iphoneos', target.name)
  derived_sources_dir = File.join(target_build_dir, 'DerivedSources')
  vars = {
    'SRCROOT' => pods_root,
    'SOURCE_ROOT' => pods_root,
    'PROJECT_DIR' => pods_root,
    'PROJECT_FILE_PATH' => project_path,
    'PODS_ROOT' => pods_root,
    'BUILD_ROOT' => build_dir,
    'BUILD_DIR' => build_dir,
    'CONFIGURATION' => 'Release',
    'CONFIGURATION_BUILD_DIR' => target_build_dir,
    'BUILT_PRODUCTS_DIR' => target_build_dir,
    'TARGET_BUILD_DIR' => target_build_dir,
    'TARGET_NAME' => target.name,
    'TARGET_TEMP_DIR' => File.join(target_build_dir, 'Intermediates'),
    'TEMP_DIR' => File.join(target_build_dir, 'Intermediates'),
    'DERIVED_FILE_DIR' => derived_sources_dir,
    'DERIVED_SOURCES_DIR' => derived_sources_dir,
    'OBJECT_FILE_DIR_normal' => File.join(target_build_dir, 'objects'),
    'EFFECTIVE_PLATFORM_NAME' => '-iphoneos',
    'PLATFORM_NAME' => 'iphoneos',
    'ARCHS' => 'arm64',
    'CURRENT_ARCH' => 'arm64',
    'NATIVE_ARCH' => 'arm64',
    'SDKROOT' => ios_sdk,
    'TOOLCHAIN_DIR' => toolchain_dir,
    'PODS_BUILD_DIR' => build_dir,
    'PODS_CONFIGURATION_BUILD_DIR' => File.join(build_dir, 'Release-iphoneos'),
    'PODS_XCFRAMEWORKS_BUILD_DIR' => File.join(build_dir, 'Release-iphoneos', 'XCFrameworkIntermediates'),
  }
  # Let xcconfig-defined variables reference each other.
  16.times do
    changed = false
    raw.each do |key, value|
      expanded = expand_value(value, vars)
      if vars[key] != expanded
        vars[key] = expanded
        changed = true
      end
    end
    break unless changed
  end

  sources = target.source_build_phase.files.filter_map do |bf|
    ref = bf.file_ref
    next unless ref
    flags = bf.settings && bf.settings['COMPILER_FLAGS'] ? list_value(bf.settings['COMPILER_FLAGS'], vars) : []
    { 'path' => ref.real_path.to_s, 'flags' => flags }
  end
  headers = target.headers_build_phase.files.filter_map do |bf|
    ref = bf.file_ref
    next unless ref
    { 'path' => ref.real_path.to_s, 'attributes' => (bf.settings && bf.settings['ATTRIBUTES'] || []) }
  end
  resources = target.resources_build_phase.files.filter_map { |bf| bf.file_ref&.real_path&.to_s }
  module_name = expand_value(raw['PRODUCT_MODULE_NAME'] || raw['PRODUCT_NAME'] || target.name, vars)
  product_name = expand_value(raw['PRODUCT_NAME'] || target.product_name || target.name, vars)
  vars['PRODUCT_MODULE_NAME'] = module_name
  vars['PRODUCT_NAME'] = product_name
  vars['FULL_PRODUCT_NAME'] = target.product_name.to_s.empty? ? product_name : target.product_name.to_s
  build_phases = target.build_phases
  sources_index = build_phases.index(target.source_build_phase) || build_phases.length
  # CocoaPods' :before_headers/:after_headers execution positions are expressed as
  # real placement around the headers phase, so the recorded index is enough to
  # reconstruct the stage. Targets without a headers phase treat everything as
  # after-headers.
  headers_phase = target.respond_to?(:headers_build_phase) ? (target.headers_build_phase rescue nil) : nil
  headers_index = headers_phase ? (build_phases.index(headers_phase) || 0) : 0
  scripts = target.shell_script_build_phases.map do |phase|
    phase_index = build_phases.index(phase) || build_phases.length
    dependency_file = phase.respond_to?(:dependency_file) ? phase.dependency_file.to_s : ''
    {
      'name' => phase.name || 'Run Script',
      'shell_path' => phase.shell_path.to_s.empty? ? '/bin/sh' : phase.shell_path.to_s,
      'script' => phase.shell_script.to_s,
      'input_paths' => (phase.input_paths || []).map { |path| expand_value(path, vars) },
      'output_paths' => (phase.output_paths || []).map { |path| expand_value(path, vars) },
      'input_file_list_paths' => (phase.input_file_list_paths || []).map { |path| expand_value(path, vars) },
      'output_file_list_paths' => (phase.output_file_list_paths || []).map { |path| expand_value(path, vars) },
      'dependency_file' => dependency_file.empty? ? '' : expand_value(dependency_file, vars),
      'order' => phase_index,
      'before_sources' => phase_index < sources_index,
      'before_headers' => phase_index < headers_index,
      'always_out_of_date' => phase.respond_to?(:always_out_of_date) && phase.always_out_of_date.to_s == '1',
      'run_only_for_deployment_postprocessing' => phase.respond_to?(:run_only_for_deployment_postprocessing) && phase.run_only_for_deployment_postprocessing.to_s == '1',
    }
  end
  expanded_settings = {}
  vars.each { |key, value| expanded_settings[key.to_s] = value.to_s }
  prefix_header = expand_value(raw['GCC_PREFIX_HEADER'], vars)
  if !prefix_header.empty? && !prefix_header.start_with?('/')
    prefix_header = File.join(pods_root, prefix_header)
  end
  {
    'name' => target.name,
    'product_type' => target.product_type,
    'product_name' => product_name,
    'module_name' => module_name,
    'swift_version' => expand_value(raw['SWIFT_VERSION'], vars),
    'deployment_target' => expand_value(raw['IPHONEOS_DEPLOYMENT_TARGET'], vars),
    'arc_enabled' => expand_value(raw['CLANG_ENABLE_OBJC_ARC'] || 'YES', vars) != 'NO',
    'prefix_header' => prefix_header,
    'dependencies' => native_dependency_names(target),
    'sources' => sources,
    'headers' => headers,
    'resources' => resources,
    # Clang consumes both Xcode settings. Some C pods (libwebp) put the pod
    # source root only in USER_HEADER_SEARCH_PATHS and include headers with
    # PODS_TARGET_SRCROOT-relative paths such as src/dec/alphai_dec.h.
    'header_search_paths' => effective_list(raw, 'HEADER_SEARCH_PATHS', vars) + effective_list(raw, 'USER_HEADER_SEARCH_PATHS', vars),
    'framework_search_paths' => effective_list(raw, 'FRAMEWORK_SEARCH_PATHS', vars),
    'library_search_paths' => effective_list(raw, 'LIBRARY_SEARCH_PATHS', vars),
    'swift_include_paths' => effective_list(raw, 'SWIFT_INCLUDE_PATHS', vars),
    'preprocessor_definitions' => effective_list(raw, 'GCC_PREPROCESSOR_DEFINITIONS', vars),
    'other_cflags' => effective_list(raw, 'OTHER_CFLAGS', vars),
    'other_cxxflags' => effective_list(raw, 'OTHER_CPLUSPLUSFLAGS', vars),
    'other_swift_flags' => effective_list(raw, 'OTHER_SWIFT_FLAGS', vars),
    'other_ldflags' => effective_list(raw, 'OTHER_LDFLAGS', vars),
    # The Pods-* target's base xcconfig is consumed by the application target.
    # Its target build settings may deliberately blank OTHER_LDFLAGS because the
    # synthetic Pods target is not itself the final executable. Keep both views.
    'base_other_ldflags' => effective_list(xc, 'OTHER_LDFLAGS', vars),
    'base_framework_search_paths' => effective_list(xc, 'FRAMEWORK_SEARCH_PATHS', vars),
    'base_library_search_paths' => effective_list(xc, 'LIBRARY_SEARCH_PATHS', vars),
    'build_settings' => expanded_settings,
    'shell_script_phases' => scripts,
  }
end.compact
puts JSON.generate({ 'pods_root' => pods_root, 'targets' => targets })
`

func resolveCocoaPodsManifest(tc toolchain.Toolchain, projectRoot string, plugins []Plugin, minOS, swiftVersion string) (podManifest, string, error) {
	pod, err := cocoaPodsExecutable()
	if err != nil {
		return podManifest{}, "", err
	}
	root := filepath.Join(projectRoot, "build", "ripley_ios", "cocoapods")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return podManifest{}, "", err
	}
	podfile := filepath.Join(root, "Podfile")
	if err := writeResolverPodfile(podfile, projectRoot, plugins, minOS, swiftVersion); err != nil {
		return podManifest{}, "", err
	}
	fmt.Println("cocoapods: resolving external pod dependencies")
	if err := run(root, nil, pod, "install", "--allow-root"); err != nil {
		return podManifest{}, "", fmt.Errorf("CocoaPods resolver failed: %w", err)
	}
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		return podManifest{}, "", err
	}
	script := filepath.Join(root, "extract_pods.rb")
	if err := os.WriteFile(script, []byte(podManifestRuby), 0o644); err != nil {
		return podManifest{}, "", err
	}
	podBuildDir := filepath.Join(root, "build")
	toolchainRoot, err := tc.SwiftToolchainRoot()
	if err != nil {
		return podManifest{}, "", err
	}
	iosSDK, err := tc.IOSSDK()
	if err != nil {
		return podManifest{}, "", err
	}
	cmd := exec.Command(ruby, script, filepath.Join(root, "Pods", "Pods.xcodeproj"))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "RIPLEY_POD_BUILD_DIR="+podBuildDir, "RIPLEY_TOOLCHAIN_DIR="+toolchainRoot, "RIPLEY_IOS_SDK="+iosSDK)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return podManifest{}, "", fmt.Errorf("extract CocoaPods target graph: %w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return podManifest{}, "", err
	}
	var manifest podManifest
	if err := json.Unmarshal(out, &manifest); err != nil {
		return podManifest{}, "", fmt.Errorf("decode CocoaPods target graph: %w", err)
	}
	manifest.ProjectRoot = projectRoot
	frameworks, linkage, err := projectPodFrameworkLinkage(projectRoot)
	if err != nil {
		return podManifest{}, "", err
	}
	if frameworks {
		if linkage == "" {
			linkage = "dynamic"
		}
		manifest.FrameworkLinkage = linkage
	}
	return manifest, podBuildDir, nil
}
