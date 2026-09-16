package ios

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ScriptPhaseIO is the resolved dependency description of a single Xcode/CocoaPods
// run-script build phase. Every path is already variable-expanded and absolute;
// file lists have been flattened into Inputs/Outputs alongside the list files
// themselves, which is how Xcode's build system treats them.
type ScriptPhaseIO struct {
	Name            string
	Script          string
	ShellPath       string
	Inputs          []string
	Outputs         []string
	DependencyFile  string
	StateFile       string
	SearchPath      string
	AlwaysOutOfDate bool
}

func (io ScriptPhaseIO) signature() string {
	hash := sha256.New()
	write := func(label, value string) {
		fmt.Fprintf(hash, "%s\x00%s\x00", label, value)
	}
	write("name", io.Name)
	write("shell", io.ShellPath)
	write("script", io.Script)
	write("dependency_file", io.DependencyFile)
	for _, list := range []struct {
		label string
		paths []string
	}{{"input", io.Inputs}, {"output", io.Outputs}} {
		sorted := append([]string(nil), list.paths...)
		sort.Strings(sorted)
		for _, path := range sorted {
			write(list.label, path)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// ScriptPhaseUpToDate reports whether a run-script phase can be skipped, using
// the same rules Xcode applies: a phase marked "always out of date", or one that
// declares no outputs, always runs. Otherwise the phase is skipped when its
// recorded signature still matches and no declared input (or dependency-file
// entry) is newer than the oldest declared output.
//
// A missing or unreadable dependency/input/output makes the phase out of date
// rather than an error; only unexpected filesystem failures are returned.
func ScriptPhaseUpToDate(io ScriptPhaseIO) (bool, error) {
	if io.AlwaysOutOfDate || len(io.Outputs) == 0 || io.StateFile == "" {
		return false, nil
	}
	recorded, err := os.ReadFile(io.StateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			return false, err
		}
		return false, nil
	}
	if strings.TrimSpace(string(recorded)) != io.signature() {
		return false, nil
	}
	var oldestOutput time.Time
	for _, output := range io.Outputs {
		info, err := os.Stat(output)
		if err != nil {
			if !os.IsNotExist(err) {
				return false, err
			}
			return false, nil
		}
		if oldestOutput.IsZero() || info.ModTime().Before(oldestOutput) {
			oldestOutput = info.ModTime()
		}
	}
	inputs := append([]string(nil), io.Inputs...)
	if io.DependencyFile != "" {
		dependencies, err := parseMakefileDependencies(io.DependencyFile)
		if err != nil {
			if !os.IsNotExist(err) {
				return false, err
			}
			// A declared dependency file that was never produced means the
			// phase has not run to completion under these settings.
			return false, nil
		}
		// The dependency file itself is an *output* of the phase that wrote it,
		// so it is legitimately newer than the declared outputs. Only its
		// contents describe real prerequisites.
		inputs = append(inputs, dependencies...)
	}
	for _, input := range inputs {
		info, err := os.Stat(input)
		if err != nil {
			if !os.IsNotExist(err) {
				return false, err
			}
			return false, nil
		}
		if info.ModTime().After(oldestOutput) {
			return false, nil
		}
	}
	return true, nil
}

// RecordScriptPhaseState stores the signature used by ScriptPhaseUpToDate.
func RecordScriptPhaseState(io ScriptPhaseIO) error {
	if io.StateFile == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(io.StateFile), 0o755); err != nil {
		return err
	}
	return os.WriteFile(io.StateFile, []byte(io.signature()+"\n"), 0o644)
}

// parseMakefileDependencies reads a compiler-style dependency file (the format
// Xcode's `dependencyFile` / CocoaPods `dependency_file` points at) and returns
// the prerequisite paths.
func parseMakefileDependencies(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	// Join escaped line continuations so each rule is one logical line.
	text = strings.ReplaceAll(text, "\\\n", " ")
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = stripMakefileComment(line)
		if strings.TrimSpace(line) == "" {
			continue
		}
		colon := -1
		for i := 0; i < len(line); i++ {
			if line[i] == ':' && (i == 0 || line[i-1] != '\\') {
				colon = i
				break
			}
		}
		if colon < 0 {
			continue
		}
		out = append(out, splitMakefileTokens(line[colon+1:])...)
	}
	return uniqueStrings(out), nil
}

func stripMakefileComment(line string) string {
	for i := 0; i < len(line); i++ {
		if line[i] == '#' && (i == 0 || line[i-1] != '\\') {
			return line[:i]
		}
	}
	return line
}

func splitMakefileTokens(value string) []string {
	var out []string
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			out = append(out, current.String())
			current.Reset()
		}
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c == '\\' && i+1 < len(value):
			next := value[i+1]
			if next == ' ' || next == '\t' || next == '#' || next == ':' || next == '\\' {
				current.WriteByte(next)
				i++
				continue
			}
			current.WriteByte(c)
		case c == '$' && i+1 < len(value) && value[i+1] == '$':
			current.WriteByte('$')
			i++
		case c == ' ' || c == '\t':
			flush()
		default:
			current.WriteByte(c)
		}
	}
	flush()
	return out
}

// ReadScriptFileList reads an .xcfilelist and returns its entries, expanded with
// the caller's build-setting expansion function.
func ReadScriptFileList(path string, expand func(string) string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if expand != nil {
			line = expand(line)
		}
		out = append(out, line)
	}
	return out, nil
}

// macOSOnlyTools are utilities that script phases routinely call but that only
// exist inside a macOS/Xcode installation. ripley runs such phases for real rather
// than skipping them silently, so when one fails this table turns an opaque
// "command not found" into an actionable diagnostic.
var macOSOnlyTools = map[string]string{
	"xcrun":                         "Xcode command-line tool locator",
	"xcodebuild":                    "Xcode build driver",
	"xcode-select":                  "Xcode toolchain selector",
	"codesign":                      "macOS code signing (ripley signs with zsign instead)",
	"codesign_allocate":             "macOS code signing helper",
	"PlistBuddy":                    "macOS property list editor",
	"plutil":                        "macOS property list utility",
	"actool":                        "Xcode asset catalog compiler",
	"ibtool":                        "Xcode Interface Builder compiler",
	"momc":                          "Xcode Core Data model compiler",
	"mapc":                          "Xcode Core Data mapping model compiler",
	"iconutil":                      "macOS iconset compiler",
	"sips":                          "macOS image processing tool",
	"ditto":                         "macOS archive/copy tool",
	"hdiutil":                       "macOS disk image tool",
	"lipo":                          "Apple universal binary tool (llvm-lipo is the portable equivalent)",
	"install_name_tool":             "Apple dylib install-name editor",
	"otool":                         "Apple Mach-O inspector (llvm-otool is the portable equivalent)",
	"swift-stdlib-tool":             "Xcode Swift runtime embedder",
	"xcstringstool":                 "Xcode string catalog compiler",
	"assetutil":                     "macOS asset utility",
	"security":                      "macOS keychain tool",
	"defaults":                      "macOS user defaults tool",
	"osascript":                     "macOS AppleScript runner",
	"launchctl":                     "macOS service manager",
	"pbcopy":                        "macOS clipboard tool",
	"textutil":                      "macOS text conversion tool",
	"tiffutil":                      "macOS TIFF tool",
	"SetFile":                       "macOS file attribute tool",
	"GetFileInfo":                   "macOS file attribute tool",
	"agvtool":                       "Xcode version tool",
	"xcodeproj":                     "CocoaPods Xcode project CLI",
	"xcpretty":                      "Xcode log formatter",
	"simctl":                        "Xcode simulator control",
	"notarytool":                    "Xcode notarization tool",
	"stapler":                       "macOS notarization stapler",
	"xip":                           "macOS xip archive tool",
	"open":                          "macOS open command",
	"say":                           "macOS speech tool",
	"mdimport":                      "macOS Spotlight importer",
	"dot_clean":                     "macOS resource fork cleaner",
	"softwareupdate":                "macOS software update tool",
	"system_profiler":               "macOS system profiler",
	"scutil":                        "macOS system configuration tool",
	"diskutil":                      "macOS disk utility",
	"networksetup":                  "macOS network configuration tool",
	"tmutil":                        "macOS Time Machine tool",
	"caffeinate":                    "macOS power assertion tool",
	"pkgbuild":                      "macOS package builder",
	"productbuild":                  "macOS product builder",
	"altool":                        "Xcode application loader",
	"iprofiler":                     "Xcode instrumentation tool",
	"instruments":                   "Xcode instruments",
	"dwarfdump":                     "Apple DWARF inspector (llvm-dwarfdump is the portable equivalent)",
	"segedit":                       "Apple Mach-O segment editor",
	"pagestuff":                     "Apple Mach-O page inspector",
	"vtool":                         "Apple Mach-O version editor",
	"bitcode_strip":                 "Apple bitcode strip tool",
	"nmedit":                        "Apple symbol table editor",
	"redo_prebinding":               "Apple prebinding tool",
	"check_dylib":                   "Apple dylib checker",
	"mig":                           "macOS Mach interface generator",
	"gethostuuid":                   "macOS host UUID tool",
	"CpMac":                         "macOS resource-fork-aware copy",
	"MvMac":                         "macOS resource-fork-aware move",
	"ResMerger":                     "macOS resource merger",
	"Rez":                           "macOS resource compiler",
	"DeRez":                         "macOS resource decompiler",
	"copypng":                       "Xcode PNG optimizer",
	"copySceneKitAssets":            "Xcode SceneKit asset copier",
	"coremlcompiler":                "Xcode Core ML compiler",
	"mlmodelc":                      "Xcode Core ML compiler",
	"metal":                         "Xcode Metal compiler",
	"metallib":                      "Xcode Metal linker",
	"intentbuilderc":                "Xcode Intents compiler",
	"appintentsmetadataprocessor":   "Xcode App Intents metadata processor",
	"appintentsnltrainingprocessor": "Xcode App Intents training processor",
	"compileStoryboard":             "Xcode storyboard compiler",
}

// MissingMacOSTools returns the macOS-only utilities a script invokes that
// cannot be resolved against searchPath (the PATH the phase actually runs with).
func MissingMacOSTools(script, searchPath string) []string {
	var missing []string
	seen := make(map[string]bool)
	for _, command := range scriptCommands(script) {
		if command == "" {
			continue
		}
		base := filepath.Base(command)
		description, ok := macOSOnlyTools[base]
		if !ok || seen[base] {
			continue
		}
		if strings.ContainsRune(command, filepath.Separator) {
			if _, err := os.Stat(command); err == nil {
				continue
			}
		} else if _, found := lookupInSearchPath(base, searchPath); found {
			continue
		}
		seen[base] = true
		missing = append(missing, fmt.Sprintf("%s (%s)", base, description))
	}
	sort.Strings(missing)
	return missing
}

// AnnotateScriptPhaseError enriches a failed script phase error with the
// macOS-only tools the script needs but that are unavailable on this host.
func AnnotateScriptPhaseError(err error, script, searchPath string) error {
	if err == nil {
		return nil
	}
	missing := MissingMacOSTools(script, searchPath)
	if len(missing) == 0 {
		return err
	}
	return fmt.Errorf("%w; this phase requires unavailable macOS tools: %s", err, strings.Join(missing, ", "))
}

func lookupInSearchPath(name, searchPath string) (string, bool) {
	for _, dir := range filepath.SplitList(searchPath) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		return candidate, true
	}
	return "", false
}

// shellControlKeywords introduce another simple command, so the token following
// them is itself a command position.
var shellControlKeywords = map[string]bool{
	"!": true, "do": true, "elif": true, "else": true, "exec": true,
	"if": true, "nohup": true, "sudo": true, "then": true, "time": true,
	"until": true, "while": true, "xargs": true, "env": true,
}

// shellProbeCommands take a command name as an argument but only test for its
// existence, so their arguments must not be reported as required tools.
var shellProbeCommands = map[string]bool{
	"command": true, "hash": true, "type": true, "which": true,
}

// scriptCommands extracts the command names a shell script invokes. It is a
// deliberately small scanner: enough to recognise command positions after
// separators, keywords, and substitutions, without pretending to be a shell.
func scriptCommands(script string) []string {
	var out []string
	tokens, positions := shellTokens(script)
	commandPosition := true
	skipSimpleCommand := false
	for i, token := range tokens {
		if positions[i] {
			commandPosition = true
			skipSimpleCommand = false
			continue
		}
		if !commandPosition {
			continue
		}
		if strings.Contains(token, "=") && !strings.HasPrefix(token, "=") {
			if name := token[:strings.IndexByte(token, '=')]; isShellName(name) {
				continue
			}
		}
		if shellControlKeywords[token] {
			continue
		}
		commandPosition = false
		if skipSimpleCommand {
			continue
		}
		if shellProbeCommands[token] {
			skipSimpleCommand = true
			continue
		}
		if token != "" {
			out = append(out, token)
		}
	}
	return out
}

func isShellName(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

// shellTokens splits a script into words, returning a parallel slice marking
// which entries are command separators rather than words.
func shellTokens(script string) ([]string, []bool) {
	var tokens []string
	var separators []bool
	var current strings.Builder
	quote := byte(0)
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			separators = append(separators, false)
			current.Reset()
		}
	}
	addSeparator := func() {
		flush()
		tokens = append(tokens, "")
		separators = append(separators, true)
	}
	for i := 0; i < len(script); i++ {
		c := script[i]
		if quote != 0 {
			if c == '\\' && quote == '"' && i+1 < len(script) {
				current.WriteByte(script[i+1])
				i++
				continue
			}
			if c == quote {
				quote = 0
				continue
			}
			// Command substitution inside double quotes still runs commands.
			if quote == '"' && c == '$' && i+1 < len(script) && script[i+1] == '(' {
				addSeparator()
				i++
				continue
			}
			current.WriteByte(c)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '\\':
			if i+1 < len(script) {
				current.WriteByte(script[i+1])
				i++
			}
		case ' ', '\t':
			flush()
		case '\n', ';', '|', '&', '(', ')', '{', '}', '`':
			addSeparator()
		case '$':
			if i+1 < len(script) && script[i+1] == '(' {
				addSeparator()
				i++
			} else {
				current.WriteByte(c)
			}
		case '<', '>':
			// Redirection target is not a command.
			flush()
			tokens = append(tokens, "")
			separators = append(separators, false)
		default:
			current.WriteByte(c)
		}
	}
	flush()
	return tokens, separators
}
