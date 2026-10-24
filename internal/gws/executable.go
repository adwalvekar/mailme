package gws

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func nativeExecutable(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	f, err := os.Open(resolved)
	if err != nil {
		return "", err
	}
	head, _ := io.ReadAll(io.LimitReader(f, 4096))
	f.Close()
	first, _, _ := strings.Cut(string(head), "\n")
	if !strings.HasPrefix(first, "#!") || !strings.Contains(first, "node") {
		return path, nil
	}
	data, _ := os.ReadFile(filepath.Join(filepath.Dir(resolved), "package.json"))
	var pkg struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(data, &pkg) == nil && pkg.Name == "@googleworkspace/cli" {
		native := filepath.Join(filepath.Dir(resolved), "bin", "gws")
		if stat, err := os.Stat(native); err == nil && stat.Mode().IsRegular() && stat.Mode().Perm()&0111 != 0 && isNative(native) {
			return native, nil
		}
	}
	return "", errors.New("gws resolves to a Node shim; use --gws-path with the installed native @googleworkspace/cli/bin/gws binary so launchd does not depend on nvm's PATH")
}

func isNative(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	switch binary.BigEndian.Uint32(magic[:]) {
	case 0x7f454c46, 0xfeedface, 0xcefaedfe, 0xfeedfacf, 0xcffaedfe, 0xcafebabe, 0xbebafeca, 0xcafebabf, 0xbfbafeca:
		return true
	}
	return false
}
