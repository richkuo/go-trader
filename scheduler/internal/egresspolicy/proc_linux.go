//go:build linux

package egresspolicy

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func enforcementPlatformSupported() error {
	return nil
}

func PeerCredentials(conn net.Conn) (PeerCred, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return PeerCred{}, fmt.Errorf("the control connection is not a Unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return PeerCred{}, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return PeerCred{}, err
	}
	if credErr != nil {
		return PeerCred{}, credErr
	}
	return PeerCred{PID: int(cred.Pid), UID: int(cred.Uid), GID: int(cred.Gid)}, nil
}

func verifyControllerPeer(conn net.Conn) error {
	cred, err := PeerCredentials(conn)
	if err != nil {
		return fmt.Errorf("read the egress controller peer credentials: %v", err)
	}
	if cred.UID != 0 {
		return fmt.Errorf("the egress control socket is served by uid %d, not root", cred.UID)
	}
	return nil
}

func NamespaceInode(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, fmt.Errorf("stat %s: %v", path, err)
	}
	return st.Ino, nil
}

func CurrentNetnsInode() (uint64, error) {
	return NamespaceInode("/proc/self/ns/net")
}

func ProcessNetnsInode(pid int) (uint64, error) {
	return NamespaceInode("/proc/" + strconv.Itoa(pid) + "/ns/net")
}

type ProcessSecurity struct {
	UID        int
	CapEff     uint64
	CapPrm     uint64
	CapAmb     uint64
	NoNewPrivs bool
	Seccomp    int
	StartTime  uint64
}

func InspectProcess(pid int) (ProcessSecurity, error) {
	var ps ProcessSecurity
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return ps, err
	}
	defer f.Close()
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "Uid":
			fields := strings.Fields(val)
			if len(fields) < 2 {
				return ps, fmt.Errorf("malformed Uid line")
			}
			ps.UID, err = strconv.Atoi(fields[1])
			if err != nil {
				return ps, err
			}
			for _, f := range fields {
				if f != fields[0] {
					return ps, fmt.Errorf("process %d has mixed real, effective, saved or filesystem uids", pid)
				}
			}
		case "CapEff":
			ps.CapEff, err = strconv.ParseUint(val, 16, 64)
		case "CapPrm":
			ps.CapPrm, err = strconv.ParseUint(val, 16, 64)
		case "CapAmb":
			ps.CapAmb, err = strconv.ParseUint(val, 16, 64)
		case "NoNewPrivs":
			ps.NoNewPrivs = val == "1"
		case "Seccomp":
			ps.Seccomp, err = strconv.Atoi(val)
		default:
			continue
		}
		if err != nil {
			return ps, fmt.Errorf("parse %s: %v", key, err)
		}
		seen[key] = true
	}
	if err := sc.Err(); err != nil {
		return ps, err
	}
	for _, k := range []string{"Uid", "CapEff", "CapPrm", "CapAmb", "NoNewPrivs"} {
		if !seen[k] {
			return ps, fmt.Errorf("/proc/%d/status has no %s line", pid, k)
		}
	}
	start, err := ProcessStartTime(pid)
	if err != nil {
		return ps, err
	}
	ps.StartTime = start
	return ps, nil
}

func ProcessStartTime(pid int) (uint64, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	s := string(data)
	end := strings.LastIndexByte(s, ')')
	if end < 0 {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(s[end+1:])
	if len(fields) < 20 {
		return 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	return strconv.ParseUint(fields[19], 10, 64)
}
