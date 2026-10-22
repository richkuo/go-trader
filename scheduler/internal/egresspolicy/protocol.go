package egresspolicy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"regexp"
	"time"
)

const (
	ProtocolVersion = 1
	ControlRoot     = "/run/go-trader-egress"
	ControlSocket   = "control.sock"
	MaxMessageBytes = 64 << 10

	OpAttest     = "attest"
	OpRestartApp = "restart-app"
	OpStatus     = "status"

	EnvProxy      = "GO_TRADER_EGRESS_PROXY"
	EnvMode       = "GO_TRADER_EGRESS_MODE"
	EnvDeployment = "GO_TRADER_EGRESS_DEPLOYMENT"
	ModeProxy     = "proxy"
)

var ErrEnforcementUnavailable = errors.New("egress enforcement is unavailable")

var ErrNamespaceMismatch = errors.New("this process is not in the enforced network namespace")

const CodeNamespaceMismatch = "namespace_mismatch"

var deploymentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

func ValidDeployment(name string) error {
	if !deploymentPattern.MatchString(name) {
		return fmt.Errorf("deployment name %q must match %s", name, deploymentPattern.String())
	}
	return nil
}

func ControlSocketPath(deployment string) string {
	return filepath.Join(ControlRoot, deployment, ControlSocket)
}

type PeerCred struct {
	PID int
	UID int
	GID int
}

type Request struct {
	Version    int    `json:"version"`
	Op         string `json:"op"`
	Deployment string `json:"deployment"`
	Role       string `json:"role,omitempty"`
}

const (
	RoleWorker = "worker"
	RoleNotify = "notify"
)

type Response struct {
	Version     int               `json:"version"`
	OK          bool              `json:"ok"`
	Error       string            `json:"error,omitempty"`
	Code        string            `json:"code,omitempty"`
	Attestation *Attestation      `json:"attestation,omitempty"`
	Status      *ControllerStatus `json:"status,omitempty"`
}

type Attestation struct {
	Deployment        string             `json:"deployment"`
	Generation        uint64             `json:"generation"`
	NetnsInode        uint64             `json:"netns_inode"`
	Proxy             Identity           `json:"proxy"`
	RelayAddress      string             `json:"relay_address"`
	UpstreamAddresses int                `json:"upstream_addresses"`
	ControllerVersion string             `json:"controller_version"`
	IssuedAt          time.Time          `json:"issued_at"`
	Role              string             `json:"role"`
	WorkerUID         int                `json:"worker_uid"`
	Notify            *NotifyAttestation `json:"notify,omitempty"`
}

type NotifyAttestation struct {
	BrokerUID    int      `json:"broker_uid"`
	Socket       string   `json:"socket"`
	ProxyAddress string   `json:"proxy_address,omitempty"`
	Destinations []string `json:"destinations"`
}

type ControllerStatus struct {
	Deployment        string    `json:"deployment"`
	Mode              string    `json:"mode"`
	Ready             bool      `json:"ready"`
	Generation        uint64    `json:"generation"`
	NetnsInode        uint64    `json:"netns_inode"`
	Proxy             Identity  `json:"proxy"`
	RelayAddress      string    `json:"relay_address"`
	Upstreams         []string  `json:"upstreams"`
	LastResolveAt     time.Time `json:"last_resolve_at,omitempty"`
	LastResolveError  string    `json:"last_resolve_error,omitempty"`
	RelayConnections  int       `json:"relay_connections"`
	RulesVerifiedAt   time.Time `json:"rules_verified_at,omitempty"`
	RulesError        string    `json:"rules_error,omitempty"`
	ControllerVersion string    `json:"controller_version"`
}

func ReadMessage(r *bufio.Reader, v any) error {
	line, err := r.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return fmt.Errorf("control message exceeds %d bytes", MaxMessageBytes)
		}
		if errors.Is(err, io.EOF) && len(line) == 0 {
			return io.EOF
		}
		return err
	}
	return json.Unmarshal(line, v)
}

func WriteMessage(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

func roundTrip(ctx context.Context, socketPath string, req Request) (*Response, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot reach the egress controller at %s: %v", ErrEnforcementUnavailable, socketPath, err)
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second)
	}
	_ = conn.SetDeadline(deadline)
	if err := verifyControllerPeer(conn); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEnforcementUnavailable, err)
	}
	if err := WriteMessage(conn, req); err != nil {
		return nil, fmt.Errorf("%w: send to the egress controller: %v", ErrEnforcementUnavailable, err)
	}
	var resp Response
	if err := ReadMessage(bufio.NewReaderSize(conn, MaxMessageBytes), &resp); err != nil {
		return nil, fmt.Errorf("%w: read from the egress controller: %v", ErrEnforcementUnavailable, err)
	}
	if resp.Version != ProtocolVersion {
		return nil, fmt.Errorf("%w: the egress controller speaks protocol %d; this build needs protocol %d", ErrEnforcementUnavailable, resp.Version, ProtocolVersion)
	}
	if !resp.OK {
		if resp.Code == CodeNamespaceMismatch {
			return nil, fmt.Errorf("%w: %w: %s", ErrEnforcementUnavailable, ErrNamespaceMismatch, resp.Error)
		}
		return nil, fmt.Errorf("%w: the egress controller refused: %s", ErrEnforcementUnavailable, resp.Error)
	}
	return &resp, nil
}

func Attest(ctx context.Context, deployment, role string, want Identity) (*Attestation, error) {
	if err := ValidDeployment(deployment); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEnforcementUnavailable, err)
	}
	if err := enforcementPlatformSupported(); err != nil {
		return nil, err
	}
	resp, err := roundTrip(ctx, ControlSocketPath(deployment), Request{Version: ProtocolVersion, Op: OpAttest, Deployment: deployment, Role: role})
	if err != nil {
		return nil, err
	}
	a := resp.Attestation
	if a == nil {
		return nil, fmt.Errorf("%w: the egress controller returned no attestation", ErrEnforcementUnavailable)
	}
	if a.Deployment != deployment || a.Role != role {
		return nil, fmt.Errorf("%w: the attestation names deployment %q role %q, not %q role %q", ErrEnforcementUnavailable, a.Deployment, a.Role, deployment, role)
	}
	if role == RoleWorker && a.Proxy != want {
		return nil, fmt.Errorf("%w: the egress controller enforces proxy %s but this process is configured for %s", ErrEnforcementUnavailable, a.Proxy, want)
	}
	if role == RoleNotify && a.Notify == nil {
		return nil, fmt.Errorf("%w: the egress controller has no notification broker policy", ErrEnforcementUnavailable)
	}
	if a.Generation == 0 {
		return nil, fmt.Errorf("%w: the egress controller has not installed its rules", ErrEnforcementUnavailable)
	}
	own, err := CurrentNetnsInode()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrEnforcementUnavailable, err)
	}
	if own != a.NetnsInode {
		return nil, fmt.Errorf("%w: %w: this process is in network namespace %d, not the enforced namespace %d", ErrEnforcementUnavailable, ErrNamespaceMismatch, own, a.NetnsInode)
	}
	return a, nil
}

func RequestRestart(ctx context.Context, deployment string) error {
	if err := ValidDeployment(deployment); err != nil {
		return err
	}
	_, err := roundTrip(ctx, ControlSocketPath(deployment), Request{Version: ProtocolVersion, Op: OpRestartApp, Deployment: deployment})
	return err
}

func QueryStatus(ctx context.Context, deployment string) (*ControllerStatus, error) {
	if err := ValidDeployment(deployment); err != nil {
		return nil, err
	}
	resp, err := roundTrip(ctx, ControlSocketPath(deployment), Request{Version: ProtocolVersion, Op: OpStatus, Deployment: deployment})
	if err != nil {
		return nil, err
	}
	if resp.Status == nil {
		return nil, fmt.Errorf("the egress controller returned no status")
	}
	return resp.Status, nil
}
