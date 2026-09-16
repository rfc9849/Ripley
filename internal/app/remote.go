package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type remote struct {
	Host string
}

func (r remote) run(command string) error {
	fmt.Printf("[%s] $ %s\n", r.Host, command)
	return run("", nil, "ssh", "-o", "BatchMode=yes", r.Host, command)
}

func (r remote) push(local, remote string) error {
	info, err := os.Stat(local)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		if err := r.run("mkdir -p " + remote); err != nil {
			return err
		}
		return run("", nil, "rsync", "-az", local, r.Host+":"+remote+"/")
	}
	return run("", nil, "rsync", "-az", "--delete", "--exclude", "build/", "--exclude", ".git/", "--exclude", ".dart_tool/", "--exclude", "ios/Flutter/Generated.xcconfig", "--exclude", "ios/Flutter/flutter_export_environment.sh", local+string(os.PathSeparator), r.Host+":"+remote)
}

func (r remote) pull(remote, local string) error {
	if err := os.MkdirAll(local, 0o755); err != nil {
		return err
	}
	return run("", nil, "rsync", "-az", r.Host+":"+remote, local+string(os.PathSeparator))
}

func buildRemote(opt buildOptions) error {
	projectDir, err := filepath.Abs(opt.Dir)
	if err != nil {
		return err
	}
	projectConfig, err := resolveIOSProject(projectDir, opt.Mode, opt.Flavor, opt.Target)
	if err != nil {
		return err
	}
	applyBundleIDOverride(&projectConfig, opt.BundleID)
	remote := remote{Host: opt.Host}
	remoteDir := "~/ripley-work/" + filepath.Base(projectDir)
	if err := remote.run("mkdir -p " + remoteDir); err != nil {
		return err
	}
	if err := remote.push(projectDir, remoteDir); err != nil {
		return err
	}

	var signingArgs []string
	if !opt.NoSign {
		material, err := resolveSigning(opt, projectConfig.BundleID)
		if err != nil {
			return err
		}
		remoteSigning := opt
		remoteSigning.Key = material.Key
		remoteSigning.Cert = material.Cert
		remoteSigning.Provision = material.Provision
		remoteSigning.Password = material.Password
		signingArgs, err = pushsigningMaterial(remote, remoteDir, remoteSigning)
		if err != nil {
			return err
		}
	}
	remoteFlutter := `if command -v flutter >/dev/null 2>&1; then command -v flutter; elif [ -x "$HOME/flutter-sdk/bin/flutter" ]; then printf '%s\n' "$HOME/flutter-sdk/bin/flutter"; else exit 127; fi`
	prepare := "cd " + remoteDir + " && FLUTTER_BIN=$(" + remoteFlutter + ") && \"$FLUTTER_BIN\" pub get"
	if err := remote.run(prepare); err != nil {
		return fmt.Errorf("remote flutter pub get failed: %w", err)
	}

	args := []string{
		"build", "ios",
		"--mode", opt.Mode,
	}
	if opt.IPA {
		args = append(args, "--ipa")
	}
	if opt.NoSign {
		args = append(args, "--no-sign")
	}
	if opt.NoPlugins {
		args = append(args, "--no-plugins")
	}
	if opt.Flavor != "" {
		args = append(args, "--flavor", opt.Flavor)
	}
	if opt.Target != "" {
		target := projectConfig.BuildSettings["FLUTTER_TARGET"]
		args = append(args, "--target", target)
	}
	if opt.BundleID != "" {
		args = append(args, "--bundle-id", opt.BundleID)
	}
	if opt.NoTreeShakeIcons {
		args = append(args, "--no-tree-shake-icons")
	}
	if opt.SplitDebugInfo != "" {
		args = append(args, "--split-debug-info", opt.SplitDebugInfo)
	}
	if opt.Obfuscate {
		args = append(args, "--obfuscate")
	}
	if opt.SaveDebugInfo {
		args = append(args, "--save-debugging-info")
	}
	args = append(args, signingArgs...)

	remoteBinary := os.Getenv("RIPLEY_REMOTE_BIN")
	if remoteBinary == "" {
		remoteBinary = "ripley"
	}
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, shellQuote(remoteBinary))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	command := "cd " + remoteDir + " && FLUTTER_BIN=$(" + remoteFlutter + ") && FLUTTER_ROOT=$(cd \"$(dirname \"$FLUTTER_BIN\")/..\" && pwd) " + strings.Join(quoted, " ")
	if err := remote.run(command); err != nil {
		return err
	}

	out := filepath.Join("build", "ripley_ios")
	if err := remote.pull(remoteDir+"/build/ripley_ios/", out); err != nil {
		return err
	}
	fmt.Printf("\nPulled build output to %s\n", out)
	return nil
}

func pushsigningMaterial(remote remote, remoteDir string, opt buildOptions) ([]string, error) {
	remoteSigningDir := remoteDir + "/.ripley-signing"
	type item struct {
		Flag string
		Path string
	}
	items := []item{
		{Flag: "--key", Path: opt.Key},
		{Flag: "--cert", Path: opt.Cert},
		{Flag: "--prov", Path: opt.Provision},
	}
	var args []string
	for _, item := range items {
		if item.Path == "" {
			continue
		}
		path, err := filepath.Abs(expandHome(item.Path))
		if err != nil {
			return nil, err
		}
		if err := remote.push(path, remoteSigningDir); err != nil {
			return nil, err
		}
		args = append(args, item.Flag, remoteSigningDir+"/"+filepath.Base(path))
	}
	if opt.Password != "" {
		args = append(args, "--password", opt.Password)
	}
	return args, nil
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func expandHome(path string) string {
	if path == "~" {
		return userHomeDir()
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(userHomeDir(), path[2:])
	}
	return path
}
