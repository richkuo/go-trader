//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
)

const (
	stRdonly      = 0x1
	stNosuid      = 0x2
	stNodev       = 0x4
	stNoexec      = 0x8
	stNoatime     = 0x400
	stNodiratime  = 0x800
	stRelatime    = 0x1000
	stNosymfollow = 0x2000

	msNosymfollow = 0x100

	sqliteShmDMSOffset = 128

	nsGetUserNS = 0xb701
)

func ledgerCaptureConfinementAvailable() error {
	return nil
}

func currentMountNamespace() (string, error) {
	return os.Readlink("/proc/self/ns/mnt")
}

func captureNSIdentity(f *os.File) (uint64, uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return 0, 0, err
	}
	return uint64(st.Dev), st.Ino, nil
}

func captureOwnsMountNamespace() error {
	uidMap, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		return fmt.Errorf("read the worker's user namespace map: %w", err)
	}
	fields := bytes.Fields(uidMap)
	if len(fields) == 3 && string(fields[0]) == "0" && string(fields[1]) == "0" && string(fields[2]) == "4294967295" {
		return fmt.Errorf("the worker is in the initial user namespace")
	}
	mnt, err := os.Open("/proc/self/ns/mnt")
	if err != nil {
		return fmt.Errorf("open the worker's mount namespace: %w", err)
	}
	defer mnt.Close()
	r, _, errno := syscall.Syscall(syscall.SYS_IOCTL, mnt.Fd(), nsGetUserNS, 0)
	if errno != 0 {
		return fmt.Errorf("read the owner of the worker's mount namespace: %w", errno)
	}
	owner := os.NewFile(r, "mount-namespace-owner")
	defer owner.Close()
	user, err := os.Open("/proc/self/ns/user")
	if err != nil {
		return fmt.Errorf("open the worker's user namespace: %w", err)
	}
	defer user.Close()
	ownerDev, ownerIno, err := captureNSIdentity(owner)
	if err != nil {
		return err
	}
	userDev, userIno, err := captureNSIdentity(user)
	if err != nil {
		return err
	}
	if ownerDev != userDev || ownerIno != userIno {
		return fmt.Errorf("the worker's mount namespace is owned by another user namespace")
	}
	return nil
}

func captureRequirePrivateMountNamespace() error {
	ns, err := currentMountNamespace()
	if err != nil {
		return fmt.Errorf("read mount namespace: %w", err)
	}
	ppid := os.Getppid()
	parentNS, readErr := os.Readlink(fmt.Sprintf("/proc/%d/ns/mnt", ppid))
	if now := os.Getppid(); now != ppid {
		return fmt.Errorf("the capture worker's parent changed (%d then %d); it is not in a private mount namespace", ppid, now)
	}
	if ns == "" {
		return fmt.Errorf("the capture worker cannot name its own mount namespace; it is not in a private mount namespace")
	}
	if readErr == nil && parentNS != "" {
		if parentNS == ns {
			return fmt.Errorf("the capture worker is not in a private mount namespace (it shares %s with its parent)", ns)
		}
		return nil
	}
	if err := captureOwnsMountNamespace(); err != nil {
		return fmt.Errorf("the capture worker cannot read its parent's mount namespace and is not in a private mount namespace of its own user namespace: %v", err)
	}
	return nil
}

func runCaptureWorker(plan captureWorkerPlan) (captureWorkerResult, bool, error) {
	payload, err := json.Marshal(plan)
	if err != nil {
		return captureWorkerResult{}, false, fmt.Errorf("encode capture plan: %w", err)
	}
	userNS := os.Geteuid() != 0
	attr := &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS, Pdeathsig: syscall.SIGKILL}
	if userNS {
		attr.Cloneflags |= syscall.CLONE_NEWUSER
		attr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Geteuid(), Size: 1}}
		attr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getegid(), Size: 1}}
	}
	cmd := exec.Command("/proc/self/exe", "export", captureWorkerTarget)
	cmd.Env = []string{}
	cmd.Stdin = bytes.NewReader(payload)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = attr
	if err := cmd.Start(); err != nil {
		how := "root with CAP_SYS_ADMIN"
		if userNS {
			how = "unprivileged user namespaces (or run as root with CAP_SYS_ADMIN)"
		}
		return captureWorkerResult{}, userNS, fmt.Errorf("cannot start the capture worker in a private mount namespace (%v); capture needs %s and refuses to read state files without a read-only source view", err, how)
	}
	if err := cmd.Wait(); err != nil {
		return captureWorkerResult{}, userNS, fmt.Errorf("capture worker failed: %v", err)
	}
	var res captureWorkerResult
	dec := json.NewDecoder(&stdout)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil {
		return captureWorkerResult{}, userNS, fmt.Errorf("decode capture worker result: %w", err)
	}
	return res, userNS, nil
}

func captureRemountFlags(statFlags int64) uintptr {
	flags := uintptr(syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_RDONLY)
	pairs := []struct {
		st int64
		ms uintptr
	}{
		{stNosuid, syscall.MS_NOSUID},
		{stNodev, syscall.MS_NODEV},
		{stNoexec, syscall.MS_NOEXEC},
		{stNoatime, syscall.MS_NOATIME},
		{stNodiratime, syscall.MS_NODIRATIME},
		{stRelatime, syscall.MS_RELATIME},
		{stNosymfollow, msNosymfollow},
	}
	for _, p := range pairs {
		if statFlags&p.st != 0 {
			flags |= p.ms
		}
	}
	return flags
}

func captureProtectReadOnly(dirs []string) error {
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make the worker's mounts private: %w", err)
	}
	for _, d := range dirs {
		if err := syscall.Mount(d, d, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("bind %s: %w", d, err)
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(d, &st); err != nil {
			return fmt.Errorf("statfs %s: %w", d, err)
		}
		if err := syscall.Mount("", d, "", captureRemountFlags(int64(st.Flags)), ""); err != nil {
			return fmt.Errorf("remount %s read-only: %w", d, err)
		}
	}
	return nil
}

func captureRequireReadOnly(path string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return fmt.Errorf("statfs %s: %w", path, err)
	}
	if int64(st.Flags)&stRdonly == 0 {
		return fmt.Errorf("%s is not on a read-only mount", path)
	}
	return nil
}

func captureShmHasLiveConnection(shm string) (bool, error) {
	f, err := os.OpenFile(shm, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	lk := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: sqliteShmDMSOffset, Len: 1}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_GETLK, &lk); err != nil {
		return false, err
	}
	return lk.Type != syscall.F_UNLCK, nil
}

func captureReadHeader(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	header := make([]byte, 100)
	if _, err := io.ReadFull(f, header); err != nil {
		return nil, fmt.Errorf("read SQLite header of %s: %w", path, err)
	}
	if !bytes.Equal(header[:16], []byte("SQLite format 3\x00")) {
		return nil, fmt.Errorf("%s is not an SQLite database file", path)
	}
	return header, nil
}

func captureSourceSidecarState(src string) (wal, shm, journal bool, err error) {
	exists := func(p string) (bool, error) {
		_, err := os.Lstat(p)
		if err == nil {
			return true, nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if wal, err = exists(src + "-wal"); err != nil {
		return
	}
	if shm, err = exists(src + "-shm"); err != nil {
		return
	}
	journal, err = exists(src + "-journal")
	return
}

func captureClassifySource(f captureWorkerFile) error {
	header, err := captureReadHeader(f.Source)
	if err != nil {
		return err
	}
	wal, shm, journal, err := captureSourceSidecarState(f.Source)
	if err != nil {
		return err
	}
	walMode := header[18] == 2 || header[19] == 2
	switch {
	case journal:
		return fmt.Errorf("%s state file %q has a rollback journal beside it; it may need recovery, which a read-only capture never performs", f.Role, f.Source)
	case walMode && !wal:
		return fmt.Errorf("%s state file %q is in WAL mode with no -wal file (no live SQLite connection, for example a stopped scheduler); a read-only open would have to create sidecars beside the source", f.Role, f.Source)
	case !walMode && (wal || shm):
		return fmt.Errorf("%s state file %q is in rollback mode but has WAL sidecars beside it", f.Role, f.Source)
	case walMode && !shm:
		return fmt.Errorf("%s state file %q has a -wal file and no -shm file; reading it needs WAL recovery", f.Role, f.Source)
	}
	if walMode {
		live, err := captureShmHasLiveConnection(f.Source + "-shm")
		if err != nil {
			return fmt.Errorf("probe %s shared memory: %w", f.Role, err)
		}
		if !live {
			return fmt.Errorf("%s state file %q has -wal and -shm files but no live SQLite connection holds them; reading it needs WAL recovery", f.Role, f.Source)
		}
	}
	return nil
}

func ledgerCaptureWorkerMain(r io.Reader) (captureWorkerResult, error) {
	var plan captureWorkerPlan
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&plan); err != nil {
		return captureWorkerResult{}, fmt.Errorf("decode capture plan: %w", err)
	}
	if err := captureRequirePrivateMountNamespace(); err != nil {
		return captureWorkerResult{}, fmt.Errorf("%v; refusing to change any mount", err)
	}
	if len(plan.Protect) == 0 || len(plan.Files) == 0 {
		return captureWorkerResult{}, fmt.Errorf("empty capture plan")
	}
	protect := append([]string{}, plan.Protect...)
	sort.Strings(protect)
	for _, d := range protect {
		if !filepath.IsAbs(d) || filepath.Clean(d) != d {
			return captureWorkerResult{}, fmt.Errorf("protected directory %q is not a clean absolute path", d)
		}
	}
	if err := captureProtectReadOnly(protect); err != nil {
		return captureWorkerResult{}, err
	}
	for _, d := range protect {
		if err := captureRequireReadOnly(d); err != nil {
			return captureWorkerResult{}, err
		}
	}
	dest := filepath.Clean(plan.DestinationDir)
	for _, d := range protect {
		if isPathWithin(dest, d) {
			return captureWorkerResult{}, fmt.Errorf("destination %q is inside protected directory %q", dest, d)
		}
	}
	var destStat syscall.Statfs_t
	if err := syscall.Statfs(dest, &destStat); err != nil {
		return captureWorkerResult{}, fmt.Errorf("statfs destination %q: %w", dest, err)
	}
	if int64(destStat.Flags)&stRdonly != 0 {
		return captureWorkerResult{}, fmt.Errorf("destination %q is on a read-only mount", dest)
	}
	for _, f := range plan.Files {
		dir := filepath.Dir(f.Source)
		covered := false
		for _, d := range protect {
			if d == dir {
				covered = true
			}
		}
		if !covered {
			return captureWorkerResult{}, fmt.Errorf("%s state file %q is outside every protected directory", f.Role, f.Source)
		}
		if filepath.Dir(f.Dest) != dest {
			return captureWorkerResult{}, fmt.Errorf("%s destination %q is outside the destination directory", f.Role, f.Dest)
		}
		info, err := os.Lstat(f.Source)
		if err != nil {
			return captureWorkerResult{}, fmt.Errorf("stat %s state file: %w", f.Role, err)
		}
		stamp, err := ledgerStampOf(info)
		if err != nil {
			return captureWorkerResult{}, err
		}
		if !info.Mode().IsRegular() || stamp.Dev != f.Dev || stamp.Ino != f.Ino || stamp.Nlink != 1 {
			return captureWorkerResult{}, fmt.Errorf("%s state file %q changed identity before capture", f.Role, f.Source)
		}
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			p := f.Source + suffix
			if _, err := os.Lstat(p); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return captureWorkerResult{}, fmt.Errorf("stat %s: %w", p, err)
			}
			if err := captureRequireReadOnly(p); err != nil {
				return captureWorkerResult{}, err
			}
		}
	}
	res := captureWorkerResult{Driver: ledgerSQLiteDriverVersion()}
	for _, f := range plan.Files {
		if err := captureClassifySource(f); err != nil {
			return captureWorkerResult{}, err
		}
		outcome, version, err := captureStateFileVacuumInto(f.Role, f.Source, f.Dest)
		if err != nil {
			return captureWorkerResult{}, err
		}
		res.SQLiteVersion = version
		res.Files = append(res.Files, outcome)
	}
	return res, nil
}
