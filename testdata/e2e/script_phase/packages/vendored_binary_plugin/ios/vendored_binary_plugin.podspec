Pod::Spec.new do |s|
  s.name = 'vendored_binary_plugin'
  s.version = '0.0.1'
  s.summary = 'Synthetic fixture for vendored binary support.'
  s.description = 'Exercises dynamic XCFramework and static vendored library support.'
  s.homepage = 'https://example.invalid'
  s.license = { :type => 'Apache-2.0' }
  s.author = { 'ripley' => 'ripley@example.invalid' }
  s.source = { :path => '.' }
  s.source_files = 'Classes/**/*.{h,m}'
  s.public_header_files = 'Classes/**/*.h'
  s.platform = :ios, '15.0'
  s.dependency 'Flutter'
  s.dependency 'GoogleUtilities/Environment'
  s.vendored_frameworks = 'Vendor/DynamicThing.xcframework'
  s.vendored_libraries = 'Vendor/libStaticThing.a'
  s.pod_target_xcconfig = {
    'HEADER_SEARCH_PATHS' => '$(inherited) "$(DERIVED_SOURCES_DIR)"'
  }
  s.script_phases = [
    {
      :name => 'Before Headers Phase',
      :execution_position => :before_headers,
      :output_files => ['${TARGET_BUILD_DIR}/before-headers.txt'],
      :script => <<-SCRIPT
set -eu
printf '%s\\n' "$TARGET_NAME:before_headers" > "$SCRIPT_OUTPUT_FILE_0"
SCRIPT
    },
    {
      :name => 'Generate Script Header',
      :execution_position => :before_compile,
      :input_files => ['${PODS_TARGET_SRCROOT}/script_input.txt'],
      :output_files => ['${DERIVED_SOURCES_DIR}/ScriptPhaseGenerated.h'],
      :script => <<-SCRIPT
set -eu
test "$CONFIGURATION" = "Release"
test "$PLATFORM_NAME" = "iphoneos"
test "$CURRENT_ARCH" = "arm64"
test "$SCRIPT_INPUT_FILE_COUNT" = "1"
test "$SCRIPT_OUTPUT_FILE_COUNT" = "1"
mkdir -p "$(dirname "$SCRIPT_OUTPUT_FILE_0")"
printf '#define SCRIPT_PHASE_VALUE %s\\n' "$(cat "$SCRIPT_INPUT_FILE_0")" > "$SCRIPT_OUTPUT_FILE_0"
SCRIPT
    },
    {
      :name => 'File List Bash Phase',
      :execution_position => :before_compile,
      :shell_path => '/bin/bash',
      :input_file_lists => ['${PODS_TARGET_SRCROOT}/script_inputs.xcfilelist'],
      :output_file_lists => ['${PODS_TARGET_SRCROOT}/script_outputs.xcfilelist'],
      :script => <<-SCRIPT
set -euo pipefail
[[ "$SCRIPT_INPUT_FILE_LIST_COUNT" == "1" ]]
[[ "$SCRIPT_OUTPUT_FILE_LIST_COUNT" == "1" ]]
grep -q 'script_input.txt' "$SCRIPT_INPUT_FILE_LIST_0"
grep -q 'file-list-declared-output.txt' "$SCRIPT_OUTPUT_FILE_LIST_0"
printf '%s\n' "$TARGET_NAME:$CURRENT_ARCH:bash" > "$TARGET_BUILD_DIR/file-list-declared-output.txt"
SCRIPT
    },
    {
      :name => 'Ruby Interpreter Phase',
      :execution_position => :after_compile,
      :shell_path => '/usr/bin/ruby',
      :output_files => ['${TARGET_BUILD_DIR}/ruby-phase.txt'],
      :script => "File.write(ENV.fetch('SCRIPT_OUTPUT_FILE_0'), ENV.fetch('TARGET_NAME') + ':ruby\\n')"
    },
    {
      :name => 'Post Compile Marker',
      :execution_position => :after_compile,
      :output_files => ['${TARGET_BUILD_DIR}/script-phase-post.txt'],
      :script => <<-SCRIPT
set -eu
test -f "$TARGET_BUILD_DIR/libvendored_binary_plugin.a"
printf '%s\\n' "$TARGET_NAME:$CONFIGURATION:$PLATFORM_NAME:$CURRENT_ARCH" > "$SCRIPT_OUTPUT_FILE_0"
SCRIPT
    }
  ]
  s.static_framework = true
end
