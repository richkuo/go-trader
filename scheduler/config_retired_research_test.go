package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func dirDigest(t *testing.T, dir string) string {
	t.Helper()
	var parts []string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		parts = append(parts, path+"="+missingOrFileDigest(t, path))
		return nil
	})
	sort.Strings(parts)
	return sha256Hex([]byte(strings.Join(parts, "\n")))
}

func missingOrFileDigest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "missing"
		}
		t.Fatal(err)
	}
	return sha256Hex(b)
}
