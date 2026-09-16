package ios

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDebugInfoGenSnapshotArgs(t *testing.T) {
	tests := []struct {
		name string
		opt  DebugInfoOptions
		// want is rendered with %SYM% standing in for the symbol file path.
		want []string
		// symbolDir is "work" for the build directory, "split" for the
		// --split-debug-info directory, or "" when no symbol file is expected.
		symbolDir string
	}{
		{
			name: "off",
			opt:  DebugInfoOptions{},
		},
		{
			name:      "save debugging info only writes into the build dir",
			opt:       DebugInfoOptions{SaveDebuggingInfo: true},
			symbolDir: "work",
			want: []string{
				"--dwarf-stack-traces",
				"--resolve-dwarf-paths",
				"--save-debugging-info=%SYM%",
			},
		},
		{
			name:      "split debug info implies saving debug info",
			opt:       DebugInfoOptions{SplitDebugInfo: "%SPLIT%"},
			symbolDir: "split",
			want: []string{
				"--dwarf-stack-traces",
				"--resolve-dwarf-paths",
				"--save-debugging-info=%SYM%",
			},
		},
		{
			name:      "obfuscate with split debug info",
			opt:       DebugInfoOptions{SplitDebugInfo: "%SPLIT%", Obfuscate: true},
			symbolDir: "split",
			want: []string{
				"--dwarf-stack-traces",
				"--resolve-dwarf-paths",
				"--save-debugging-info=%SYM%",
				"--obfuscate",
			},
		},
		{
			name:      "split debug info wins over the build dir",
			opt:       DebugInfoOptions{SaveDebuggingInfo: true, SplitDebugInfo: "%SPLIT%"},
			symbolDir: "split",
			want: []string{
				"--dwarf-stack-traces",
				"--resolve-dwarf-paths",
				"--save-debugging-info=%SYM%",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			work := filepath.Join(root, "build")
			// A nested, not-yet-existing path proves the directory is created.
			split := filepath.Join(root, "symbols", "v1.2.3")

			opt := tc.opt
			if opt.SplitDebugInfo != "" {
				opt.SplitDebugInfo = strings.ReplaceAll(opt.SplitDebugInfo, "%SPLIT%", split)
			}

			got, err := GenSnapshotDebugArgs(opt, work, "Runner")
			if err != nil {
				t.Fatalf("GenSnapshotDebugArgs: %v", err)
			}

			var symbolDir string
			switch tc.symbolDir {
			case "work":
				symbolDir = work
			case "split":
				symbolDir = split
			}

			want := make([]string, 0, len(tc.want))
			for _, arg := range tc.want {
				want = append(want, strings.ReplaceAll(arg, "%SYM%",
					filepath.Join(symbolDir, "app.ios-arm64.symbols")))
			}

			if len(got) != len(want) {
				t.Fatalf("args = %q, want %q", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("arg %d = %q, want %q", i, got[i], want[i])
				}
			}

			if symbolDir == "" {
				if dirExists(split) {
					t.Errorf("created %s without a debug info request", split)
				}
				return
			}
			if !dirExists(symbolDir) {
				t.Errorf("symbol directory %s was not created", symbolDir)
			}
		})
	}
}

func TestDebugInfoObfuscateRequiresSplitDebugInfo(t *testing.T) {
	// flutter_tools exits with exactly this constraint; without the symbol
	// file the identifier mapping would be unrecoverable.
	work := t.TempDir()
	args, err := GenSnapshotDebugArgs(DebugInfoOptions{Obfuscate: true}, work, "Runner")
	if err == nil {
		t.Fatalf("expected an error, got args %q", args)
	}
	for _, want := range []string{"--obfuscate", "--split-debug-info"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	// SaveDebuggingInfo is not a substitute: the flag pairing is what Flutter
	// enforces, so obfuscation must still be refused.
	if _, err := GenSnapshotDebugArgs(DebugInfoOptions{Obfuscate: true, SaveDebuggingInfo: true}, work, "Runner"); err == nil {
		t.Error("--obfuscate with only --save-debugging-info must be rejected")
	}
}

func TestDebugInfoSymbolFileNamedForArchNotProduct(t *testing.T) {
	// A single symbol name per architecture is what `flutter symbolize`
	// expects; deriving it from the product would break that contract.
	split := filepath.Join(t.TempDir(), "symbols")
	first, err := GenSnapshotDebugArgs(DebugInfoOptions{SplitDebugInfo: split}, "", "Runner")
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenSnapshotDebugArgs(DebugInfoOptions{SplitDebugInfo: split}, "", "MyCoolApp")
	if err != nil {
		t.Fatal(err)
	}

	want := "--save-debugging-info=" + filepath.Join(split, "app.ios-arm64.symbols")
	for _, args := range [][]string{first, second} {
		if len(args) == 0 || args[len(args)-1] != want {
			t.Fatalf("args %q do not end with %q", args, want)
		}
	}
}

func TestDebugInfoRejectsSaveDebuggingInfoWithoutDestination(t *testing.T) {
	if _, err := GenSnapshotDebugArgs(DebugInfoOptions{SaveDebuggingInfo: true}, "", "Runner"); err == nil {
		t.Fatal("expected an error when no directory is available for the symbol file")
	}
}
