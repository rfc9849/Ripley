package ios

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"ripley/internal/toolchain"
)

// previewMacroUseRE matches the two spellings that reach the SDK's
// PreviewsMacros plugin: the freestanding `#Preview`/`#Previewable` macro
// expression and the `@Previewable` attribute form. A comment or string
// literal matching is harmless — it only costs building the stub plugin.
var previewMacroUseRE = regexp.MustCompile(`#Preview\b|#Previewable\b|@Previewable\b`)

// previewsMacrosSource is a stub implementation of the PreviewsMacros compiler
// plugin. Xcode ships it inside the toolchain; the Linux toolchain does not,
// yet the iOS SDK's SwiftUI/WidgetKit/UIKit swiftinterfaces declare #Preview
// and #Previewable as #externalMacro(module: "PreviewsMacros", ...), so any
// source using them fails to compile without a plugin providing those types.
//
// The stub expands every preview macro to nothing, which is exactly what a
// device build needs: previews exist only for the Xcode canvas and produce no
// runtime code. It speaks the plugin protocol directly through the toolchain's
// prebuilt SwiftCompilerPluginMessageHandling module, so it needs no
// swift-syntax checkout.
const previewsMacrosSource = `import SwiftSyntax
import SwiftSyntaxMacros
@_spi(PluginMessage) import SwiftCompilerPluginMessageHandling

public struct SwiftUIView: DeclarationMacro {
  public static func expansion(
    of node: some FreestandingMacroExpansionSyntax,
    in context: some MacroExpansionContext
  ) throws -> [DeclSyntax] {
    []
  }
}

public struct Common: DeclarationMacro {
  public static func expansion(
    of node: some FreestandingMacroExpansionSyntax,
    in context: some MacroExpansionContext
  ) throws -> [DeclSyntax] {
    []
  }
}

public struct KitViewMacro: DeclarationMacro {
  public static func expansion(
    of node: some FreestandingMacroExpansionSyntax,
    in context: some MacroExpansionContext
  ) throws -> [DeclSyntax] {
    []
  }
}

public struct Previewable: PeerMacro {
  public static func expansion(
    of node: AttributeSyntax,
    providingPeersOf declaration: some DeclSyntaxProtocol,
    in context: some MacroExpansionContext
  ) throws -> [DeclSyntax] {
    []
  }
}

struct Provider: PluginProvider {
  func resolveMacro(moduleName: String, typeName: String) throws -> Macro.Type {
    switch (moduleName, typeName) {
    case ("PreviewsMacros", "SwiftUIView"): return SwiftUIView.self
    case ("PreviewsMacros", "Common"): return Common.self
    case ("PreviewsMacros", "KitViewMacro"): return KitViewMacro.self
    case ("PreviewsMacros", "Previewable"): return Previewable.self
    default: throw PluginLookupError(moduleName: moduleName, typeName: typeName)
    }
  }
}

struct PluginLookupError: Error, CustomStringConvertible {
  var moduleName: String
  var typeName: String
  var description: String { "macro implementation type \(moduleName).\(typeName) not found in PreviewsMacros stub" }
}

let connection = try StandardIOMessageConnection()
let listener = CompilerPluginMessageListener(connection: connection, provider: Provider())
try listener.main()
`

// previewsMacrosPlugin returns the `-load-plugin-executable` argument for the
// stub PreviewsMacros plugin when any of the given Swift sources uses a preview
// macro, building the plugin for the host on first use. It returns "" when no
// source needs it.
func previewsMacrosPlugin(tc toolchain.Toolchain, sources []string) (string, error) {
	needs := false
	for _, source := range sources {
		if !strings.EqualFold(filepath.Ext(source), ".swift") {
			continue
		}
		data, err := os.ReadFile(source)
		if err != nil {
			continue
		}
		if previewMacroUseRE.Match(data) {
			needs = true
			break
		}
	}
	if !needs {
		return "", nil
	}

	dir := filepath.Join(tc.Root, "previews-macros")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	source := filepath.Join(dir, "main.swift")
	if data, err := os.ReadFile(source); err != nil || string(data) != previewsMacrosSource {
		if err := os.WriteFile(source, []byte(previewsMacrosSource), 0o644); err != nil {
			return "", err
		}
	}
	executable := filepath.Join(dir, "PreviewsMacros")
	if fresh, err := swiftPMExecutableIsFresh(executable, []string{source}); err != nil {
		return "", err
	} else if fresh {
		return executable + "#PreviewsMacros", nil
	}

	swiftc, err := swiftCompiler(tc)
	if err != nil {
		return "", err
	}
	hostModules, err := swiftPMHostPluginModules(tc)
	if err != nil {
		return "", err
	}
	args := []string{
		"-I", hostModules, "-L", hostModules,
		"-lSwiftSyntax", "-lSwiftSyntaxMacros", "-lSwiftCompilerPluginMessageHandling",
		"-Xlinker", "-rpath", "-Xlinker", hostModules,
		"-O", source, "-o", executable,
	}
	if err := run("", nil, swiftc, args...); err != nil {
		return "", err
	}
	return executable + "#PreviewsMacros", nil
}
