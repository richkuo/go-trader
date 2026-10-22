//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"trading-scheduler/internal/egresspolicy"
)

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if !envKeyPattern.MatchString(key) {
			continue
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' || val[0] == '\'' && val[len(val)-1] == '\'') {
			if val[0] == '"' {
				if unq, err := strconv.Unquote(val); err == nil {
					val = unq
				} else {
					val = val[1 : len(val)-1]
				}
			} else {
				val = val[1 : len(val)-1]
			}
		}
		out[key] = val
	}
	return out, sc.Err()
}

func runIdentity(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: identity ENV_FILE")
	}
	env, err := parseEnvFile(args[0])
	if err != nil {
		return err
	}
	raw := env[egresspolicy.EnvProxy]
	if raw == "" {
		return fmt.Errorf("%s is not set in %s", egresspolicy.EnvProxy, args[0])
	}
	ep, err := egresspolicy.ParseProxyURL(raw)
	if err != nil {
		return fmt.Errorf("%s in %s is invalid: %v", egresspolicy.EnvProxy, args[0], egresspolicy.NewRedactor(ep).Redact(err.Error()))
	}
	data, _ := json.Marshal(ep.Identity())
	fmt.Println(string(data))
	return nil
}

var notifyEnvKeys = []string{"DISCORD_BOT_TOKEN", "TELEGRAM_BOT_TOKEN", "GO_TRADER_GITHUB_TOKEN", "GITHUB_TOKEN", "GO_TRADER_NOTIFY_GITHUB_REPOS"}

func runExtractNotifyEnv(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: extract-notify-env APP_ENV_FILE OUT_FILE GROUP_ID")
	}
	src, dst := args[0], args[1]
	gid, err := strconv.Atoi(args[2])
	if err != nil || gid <= 0 {
		return fmt.Errorf("group id %q is invalid", args[2])
	}
	if !filepath.IsAbs(dst) {
		return fmt.Errorf("output path must be absolute")
	}
	env, err := parseEnvFile(src)
	if err != nil {
		return err
	}
	var lines []string
	var copied []string
	for _, k := range notifyEnvKeys {
		if v, ok := env[k]; ok && v != "" {
			lines = append(lines, k+"="+strconv.Quote(v))
			copied = append(copied, k)
		}
	}
	sort.Strings(copied)
	if err := writeFileAtomic(dst, []byte(strings.Join(lines, "\n")+"\n"), 0o640); err != nil {
		return err
	}
	if err := os.Chown(dst, 0, gid); err != nil {
		return err
	}
	fmt.Printf("wrote %s with %d notification keys: %s\n", dst, len(copied), strings.Join(copied, ", "))
	return nil
}
