package ios

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"howett.net/plist"
)

func run(cwd string, env []string, name string, args ...string) error {
	fmt.Printf("+ %s", name)
	for _, arg := range args {
		fmt.Printf(" %s", arg)
	}
	fmt.Println()

	cmd := exec.Command(name, args...)
	cmd.Dir = cwd
	if env != nil {
		cmd.Env = env
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, st.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// replaceFile copies src to dst while replacing an existing destination entry.
// Framework staging runs more than once (before and after Swift compilation),
// and CocoaPods public headers may intentionally be read-only. Unlinking first
// avoids O_TRUNC failing on a read-only file while preserving the source mode on
// the newly created copy.
func replaceFile(src, dst string) error {
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	return copyFile(src, dst)
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			return os.Symlink(link, target)
		}
		if d.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFile(path, target)
	})
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func writePlist(path string, value any) error {
	data, err := plist.Marshal(value, plist.XMLFormat)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func maxVersion(a, b string) string {
	parts := func(value string) []int {
		fields := strings.Split(value, ".")
		out := make([]int, len(fields))
		for i, field := range fields {
			out[i], _ = strconv.Atoi(field)
		}
		return out
	}
	left, right := parts(a), parts(b)
	for i := 0; i < len(left) || i < len(right); i++ {
		var l, r int
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		if l > r {
			return a
		}
		if r > l {
			return b
		}
	}
	return a
}
