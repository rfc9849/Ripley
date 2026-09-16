package ios

import "ripley/internal/toolchain"

// This file exposes the iOS cross-compilation environment that the pod and app
// host builders compute privately, so other packages can compile and link
// additional Mach-O products (app extensions) without duplicating the
// conventions. The critical piece is commonSwiftFlags' `-resource-dir`, which
// points at the pruned hybrid Linux/iOS Swift resource directory materialized by
// iosResourceDir; hardcoding that path elsewhere would silently diverge.

// SwiftIOSCompileFlags returns the flags every arm64-iOS Swift compile needs:
// target triple, SDK, the staged Swift resource directory, and optimization and
// module settings.
func SwiftIOSCompileFlags(tc toolchain.Toolchain, minOS string) ([]string, error) {
	return commonSwiftFlags(tc, minOS)
}

// IOSFrameworkSearch returns the `-F` framework search flags for a device build.
func IOSFrameworkSearch(tc toolchain.Toolchain) ([]string, error) {
	return frameworkSearch(tc)
}

// IOSLibSearch returns the `-L` library search flags for a device link.
func IOSLibSearch(tc toolchain.Toolchain) ([]string, error) {
	return libSearch(tc)
}

// SwiftCompilerPath returns the swiftc that can target arm64-apple-ios.
func SwiftCompilerPath(tc toolchain.Toolchain) (string, error) {
	return swiftCompiler(tc)
}

// ClangPath returns the clang used for iOS C-family compilation.
func ClangPath(tc toolchain.Toolchain) (string, error) {
	return pluginClang(tc)
}
