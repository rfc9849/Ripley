package ios

import "testing"

// A pure C++ pod translation unit must never be compiled with -fmodules.
// Apple's SDK headers wrap C declarations in `extern "C"`, and clang treats a
// module import inside a language-linkage specification as a hard error
// ("import of C++ module 'Darwin.Mach.mach_types' appears within extern \"C\"
// language linkage specification"). Sentry's SentryCrashMonitor_CPPException.cpp
// reproduces this: it reaches <mach/mach_types.h> and <stdint.h> through
// SentryCrashMachineContext_Apple.h, which opens `extern "C"` before including
// them. Xcode never hits this because CLANG_ENABLE_MODULES is applied to C,
// ObjC and ObjC++ only.
func TestCocoaPodsCXXSourcesDropModules(t *testing.T) {
	for _, path := range []string{
		"Sources/SentryCrash/Recording/Monitors/SentryCrashMonitor_CPPException.cpp",
		"src/backtrace.cc",
		"src/thread.cxx",
		"src/legacy.c++",
		"src/legacy.cp",
	} {
		if podTUSupportsModules(podSourceLanguage(path)) {
			t.Errorf("%s: -fmodules must be dropped for a C++ translation unit", path)
		}
	}
}

// C, ObjC and ObjC++ keep modules: pods routinely rely on @import there, and
// Xcode compiles those languages with CLANG_ENABLE_MODULES=YES.
func TestCocoaPodsNonCXXSourcesKeepModules(t *testing.T) {
	for _, path := range []string{
		"Sources/Sentry/SentryClient.m",
		"Sources/SentryCrash/Recording/SentryCrashReport.c",
		"Sources/Profiling/SentryProfiler.mm",
	} {
		if !podTUSupportsModules(podSourceLanguage(path)) {
			t.Errorf("%s: -fmodules must be kept for a C/ObjC/ObjC++ translation unit", path)
		}
	}
}

// OTHER_CPLUSPLUSFLAGS applies to C++ and ObjC++ but not to C or ObjC, matching
// Xcode. Passing e.g. -std=c++14 to a .c TU makes clang reject the file.
func TestCocoaPodsCXXFlagsOnlyForCXXDialects(t *testing.T) {
	cases := map[string]bool{
		"a.cpp": true,
		"a.cc":  true,
		"a.cxx": true,
		"a.mm":  true,
		"a.m":   false,
		"a.c":   false,
	}
	for path, want := range cases {
		if got := podTUUsesCXXFlags(podSourceLanguage(path)); got != want {
			t.Errorf("%s: OTHER_CPLUSPLUSFLAGS applied = %v, want %v", path, got, want)
		}
	}
}

// Only compilable TUs reach clang; headers and unrelated pod files must not be
// mistaken for sources.
func TestCocoaPodsNonSourceFilesAreNotTranslationUnits(t *testing.T) {
	for _, path := range []string{"Sentry.h", "module.modulemap", "README.md", "Info.plist"} {
		if podSourceLanguage(path) != podTUUnknown {
			t.Errorf("%s: must not be classified as a translation unit", path)
		}
	}
}
