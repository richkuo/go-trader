package egresspolicy

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	SchemeHTTP    = "http"
	SchemeHTTPS   = "https"
	SchemeSOCKS5  = "socks5"
	SchemeSOCKS5H = "socks5h"
)

var ErrInvalidProxy = errors.New("invalid egress proxy")

type Endpoint struct {
	Scheme   string
	Host     string
	Port     int
	Username string
	Password string
	HasAuth  bool
	raw      string
}

func (e Endpoint) HostPort() string {
	return net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
}

func (e Endpoint) Redacted() string {
	return e.Scheme + "://" + e.HostPort()
}

func (e Endpoint) Identity() Identity {
	return Identity{Scheme: e.Scheme, Host: e.Host, Port: e.Port}
}

func (e Endpoint) IsSOCKS() bool {
	return e.Scheme == SchemeSOCKS5 || e.Scheme == SchemeSOCKS5H
}

func (e Endpoint) RequestsURL() string {
	scheme := e.Scheme
	if scheme == SchemeSOCKS5 {
		scheme = SchemeSOCKS5H
	}
	u := url.URL{Scheme: scheme, Host: e.HostPort()}
	if e.HasAuth {
		if e.Password != "" {
			u.User = url.UserPassword(e.Username, e.Password)
		} else {
			u.User = url.User(e.Username)
		}
	}
	return u.String()
}

func (e Endpoint) Raw() string {
	return e.raw
}

func (e Endpoint) Secrets() []string {
	var out []string
	add := func(s string) {
		if s == "" {
			return
		}
		out = append(out, s)
		if esc := url.QueryEscape(s); esc != s {
			out = append(out, esc)
		}
		if esc := url.PathEscape(s); esc != s {
			out = append(out, esc)
		}
	}
	add(e.raw)
	if e.HasAuth {
		add(e.RequestsURL())
		add(e.Username + ":" + e.Password)
		add(e.Password)
		if len(e.Username) >= 3 {
			add(e.Username)
		}
	}
	return out
}

type Identity struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
}

func (i Identity) HostPort() string {
	return net.JoinHostPort(i.Host, strconv.Itoa(i.Port))
}

func (i Identity) String() string {
	return i.Scheme + "://" + i.HostPort()
}

func (i Identity) Validate() error {
	switch i.Scheme {
	case SchemeHTTP, SchemeHTTPS, SchemeSOCKS5, SchemeSOCKS5H:
	default:
		return fmt.Errorf("%w: unsupported scheme %q", ErrInvalidProxy, i.Scheme)
	}
	if err := validateHost(i.Host); err != nil {
		return err
	}
	if i.Port < 1 || i.Port > 65535 {
		return fmt.Errorf("%w: port %d is outside 1-65535", ErrInvalidProxy, i.Port)
	}
	return nil
}

func ParseProxyURL(raw string) (Endpoint, error) {
	if raw == "" {
		return Endpoint{}, fmt.Errorf("%w: the proxy URL is empty", ErrInvalidProxy)
	}
	for _, r := range raw {
		if r <= 0x20 || r == 0x7f {
			return Endpoint{}, fmt.Errorf("%w: the proxy URL contains whitespace or a control character", ErrInvalidProxy)
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Endpoint{}, fmt.Errorf("%w: the proxy URL does not parse", ErrInvalidProxy)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case SchemeHTTP, SchemeHTTPS, SchemeSOCKS5, SchemeSOCKS5H:
	case "":
		return Endpoint{}, fmt.Errorf("%w: the proxy URL has no scheme (use http, https, socks5 or socks5h)", ErrInvalidProxy)
	default:
		return Endpoint{}, fmt.Errorf("%w: unsupported or missing scheme (use http, https, socks5 or socks5h)", ErrInvalidProxy)
	}
	if u.Opaque != "" {
		return Endpoint{}, fmt.Errorf("%w: the proxy URL must have the form scheme://host:port", ErrInvalidProxy)
	}
	if u.Path != "" && u.Path != "/" {
		return Endpoint{}, fmt.Errorf("%w: the proxy URL must not have a path", ErrInvalidProxy)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return Endpoint{}, fmt.Errorf("%w: the proxy URL must not have a query", ErrInvalidProxy)
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return Endpoint{}, fmt.Errorf("%w: the proxy URL must not have a fragment", ErrInvalidProxy)
	}
	host := u.Hostname()
	if err := validateHost(host); err != nil {
		return Endpoint{}, err
	}
	portText := u.Port()
	if portText == "" {
		return Endpoint{}, fmt.Errorf("%w: the proxy URL needs an explicit port", ErrInvalidProxy)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return Endpoint{}, fmt.Errorf("%w: the proxy port is not a number in 1-65535", ErrInvalidProxy)
	}
	ep := Endpoint{Scheme: scheme, Host: strings.ToLower(host), Port: port, raw: raw}
	if u.User != nil {
		ep.HasAuth = true
		ep.Username = u.User.Username()
		ep.Password, _ = u.User.Password()
		if ep.Username == "" {
			return Endpoint{}, fmt.Errorf("%w: the proxy credentials have an empty user name", ErrInvalidProxy)
		}
		if ep.IsSOCKS() && (len(ep.Username) > 255 || len(ep.Password) > 255) {
			return Endpoint{}, fmt.Errorf("%w: SOCKS5 credentials are limited to 255 bytes each", ErrInvalidProxy)
		}
	}
	return ep, nil
}

func validateHost(host string) error {
	if host == "" {
		return fmt.Errorf("%w: the proxy URL has no host", ErrInvalidProxy)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsUnspecified() || ip.IsMulticast() {
			return fmt.Errorf("%w: the proxy host is an unspecified or multicast address", ErrInvalidProxy)
		}
		return nil
	}
	if len(host) > 253 {
		return fmt.Errorf("%w: the proxy host name is too long", ErrInvalidProxy)
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("%w: the proxy host name is not a valid DNS name", ErrInvalidProxy)
		}
		for i, r := range label {
			ok := r == '-' && i > 0 && i < len(label)-1 ||
				r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_'
			if !ok {
				return fmt.Errorf("%w: the proxy host name is not a valid DNS name", ErrInvalidProxy)
			}
		}
	}
	return nil
}

type Redactor struct {
	secrets []string
}

func NewRedactor(ep Endpoint) *Redactor {
	secrets := ep.Secrets()
	sort.SliceStable(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	return &Redactor{secrets: secrets}
}

func (r *Redactor) Redact(s string) string {
	if r == nil {
		return s
	}
	for _, secret := range r.secrets {
		if secret != "" && strings.Contains(s, secret) {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return s
}

func (r *Redactor) Error(err error) error {
	if err == nil || r == nil {
		return err
	}
	msg := r.Redact(err.Error())
	if msg == err.Error() {
		return err
	}
	return &redactedError{msg: msg, cause: err}
}

type redactedError struct {
	msg   string
	cause error
}

func (e *redactedError) Error() string { return e.msg }

func (e *redactedError) Is(target error) bool {
	return errors.Is(e.cause, target)
}
