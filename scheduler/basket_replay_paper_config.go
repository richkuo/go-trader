package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

const pairResearchEgressMaxBytes = 64 << 10

var egressResearchSubcommands = map[string]bool{
	"basket-replay-paper": true,
}

type pairResearchEgress struct {
	Path        string        `json:"path"`
	SHA256      string        `json:"sha256"`
	SizeBytes   int64         `json:"size_bytes"`
	Device      uint64        `json:"device"`
	Inode       uint64        `json:"inode"`
	DecodedKeys []string      `json:"decoded_keys"`
	Egress      *EgressConfig `json:"-"`
}

var researchEgressAdmission *pairResearchEgress

var pairResearchDefaultConfigPaths = []string{"scheduler/config.json"}

func pairResearchConfigArg(args []string) (string, error) {
	var found []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--config" || a == "-config":
			if i+1 >= len(args) {
				return "", fmt.Errorf("--config needs an absolute research egress file")
			}
			found = append(found, args[i+1])
			i++
		case strings.HasPrefix(a, "--config="):
			found = append(found, strings.TrimPrefix(a, "--config="))
		case strings.HasPrefix(a, "-config="):
			found = append(found, strings.TrimPrefix(a, "-config="))
		case a == "--":
			i = len(args)
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("--config is required: isolated paper reads public venues only through a separate, checked research egress file and never through the default or a deployed configuration")
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("--config is given %d times; exactly one research egress file is read", len(found))
}

func pairStatIdentity(info os.FileInfo) (uint64, uint64, uint64, uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), uint64(st.Nlink), st.Uid, true
}

func admitResearchEgressConfig(args []string) (*pairResearchEgress, error) {
	path, err := pairResearchConfigArg(args)
	if err != nil {
		return nil, err
	}
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("--config %q must be an absolute, clean path", path)
	}
	for _, p := range basketReplayDeployedPrefixes {
		if path == p || strings.HasPrefix(path, p+string(os.PathSeparator)) {
			return nil, fmt.Errorf("--config %q is inside the deployed path %s; isolated paper never reads deployed configuration", path, p)
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("--config %q is unreadable: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("--config %q is a symbolic link; give the real file", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("--config %q is not a regular file", path)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("--config %q does not resolve: %w", path, err)
	}
	if real != path {
		return nil, fmt.Errorf("--config %q resolves through a symbolic link to %q; give the real path", path, real)
	}
	dev, ino, nlink, uid, ok := pairStatIdentity(info)
	if !ok {
		return nil, fmt.Errorf("--config %q has no file identity on this platform", path)
	}
	if int(uid) != os.Getuid() {
		return nil, fmt.Errorf("--config %q is owned by uid %d, not this user", path, uid)
	}
	if nlink != 1 {
		return nil, fmt.Errorf("--config %q has %d hard links; a research egress file must be one unshared file", path, nlink)
	}
	for _, d := range pairResearchDefaultConfigPaths {
		abs, aerr := filepath.Abs(d)
		if aerr != nil {
			continue
		}
		if abs == path {
			return nil, fmt.Errorf("--config %q is the default configuration; isolated paper uses its own research egress file", path)
		}
		dinfo, serr := os.Stat(abs)
		if serr != nil {
			continue
		}
		if dd, di, _, _, ok := pairStatIdentity(dinfo); ok && dd == dev && di == ino {
			return nil, fmt.Errorf("--config %q is the same file as the default configuration %s", path, d)
		}
		if dreal, rerr := filepath.EvalSymlinks(abs); rerr == nil && dreal == path {
			return nil, fmt.Errorf("--config %q is the target of the default configuration link %s", path, d)
		}
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("--config %q could not be opened without following links: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	finfo, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("--config %q: %w", path, err)
	}
	fdev, fino, fnlink, fuid, ok := pairStatIdentity(finfo)
	if !ok || fdev != dev || fino != ino || fnlink != 1 || fuid != uid || !finfo.Mode().IsRegular() {
		return nil, fmt.Errorf("--config %q changed between its checks and its open; the opened file is not the checked file", path)
	}
	blob, err := io.ReadAll(io.LimitReader(f, pairResearchEgressMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("--config %q: %w", path, err)
	}
	if len(blob) > pairResearchEgressMaxBytes {
		return nil, fmt.Errorf("--config %q is above the %d-byte bound of a research egress file", path, pairResearchEgressMaxBytes)
	}
	if again, err := os.Lstat(path); err != nil || again.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("--config %q was replaced while it was read", path)
	} else if adev, aino, _, _, ok := pairStatIdentity(again); !ok || adev != dev || aino != ino {
		return nil, fmt.Errorf("--config %q was replaced while it was read; the read file and the path no longer agree", path)
	}
	tree, err := pairStrictParse(bytes.TrimSpace(blob))
	if err != nil {
		return nil, fmt.Errorf("--config %q is not one strict JSON object: %w", path, err)
	}
	obj, ok := tree.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("--config %q is not a JSON object", path)
	}
	var extra []string
	for k := range obj {
		if k != "egress" {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		return nil, fmt.Errorf("--config %q holds %s; a research egress file holds only the egress block, and strategy and risk configuration stay in the isolated specification", path, strings.Join(extra, ", "))
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(blob, &root); err != nil {
		return nil, fmt.Errorf("--config %q: %w", path, err)
	}
	if errs := egressConfigErrors(root["egress"]); len(errs) > 0 {
		return nil, fmt.Errorf("--config %q: %s", path, strings.Join(errs, "; "))
	}
	out := &pairResearchEgress{Path: path, SHA256: sha256Hex(blob), SizeBytes: int64(len(blob)), Device: dev, Inode: ino, DecodedKeys: []string{}}
	if raw, ok := root["egress"]; ok && string(raw) != "null" {
		var ec EgressConfig
		if err := json.Unmarshal(raw, &ec); err != nil {
			return nil, fmt.Errorf("--config %q egress: %w", path, err)
		}
		out.Egress = &ec
		out.DecodedKeys = []string{"egress"}
	}
	return out, nil
}

func gateResearchEgress(sub string, args []string, out io.Writer) (int, bool) {
	adm, err := admitResearchEgressConfig(args)
	if err != nil {
		return emitPairPaperRefusal(out, refuseReplay("config_refused", err.Error())), true
	}
	if err := establishProcessEgress(context.Background(), adm.Egress, true); err != nil {
		return emitPairPaperRefusal(out, refuseReplay("egress_refused", processEgress.redact(err.Error()))), true
	}
	researchEgressAdmission = adm
	return 0, false
}

func emitPairPaperRefusal(out io.Writer, err error) int {
	o, code := pairReplayErrorOutput(err)
	if b, merr := json.Marshal(o); merr == nil {
		fmt.Fprintln(out, string(b))
	}
	fmt.Fprintf(os.Stderr, "[basket-replay-paper] refused: %v\n", err)
	return code
}
