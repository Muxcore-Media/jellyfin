package internal

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	authv1 "github.com/Muxcore-Media/core/proto/gen/muxcore/auth/v1"
)

// ADR-0019: end-user identity comes from a bearer token resolved through the
// identity provider (auth-local), never from caller-supplied headers.

const (
	envTrustCallerHeader = "JELLYFIN_TRUST_CALLER_HEADER"
	identityCacheTTL     = 30 * time.Second
	identityCacheMax     = 1024
)

// Identity is the authenticated end user behind a bearer token.
type Identity struct {
	ID    string
	Roles []string
}

// IdentityResolver resolves a bearer token to an identity. It returns
// (nil, nil) when the token is unknown.
type IdentityResolver interface {
	ResolveToken(ctx context.Context, token string) (*Identity, error)
}

var errNoIdentity = errors.New("invalid token")

func bearerToken(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// legacyHeaderTrusted reports whether header-only identity is allowed: it
// requires both MUXCORE_INSECURE_DISABLE_TLS=true and JELLYFIN_TRUST_CALLER_HEADER=1.
func legacyHeaderTrusted() bool {
	return os.Getenv("MUXCORE_INSECURE_DISABLE_TLS") == "true" &&
		os.Getenv(envTrustCallerHeader) == "1"
}

// cachingResolver caches successful resolutions for identityCacheTTL, keyed by
// the token's SHA-256.
type cachingResolver struct {
	next IdentityResolver
	mu   sync.Mutex
	m    map[string]cachedIdentity
	now  func() time.Time
}

type cachedIdentity struct {
	id  Identity
	exp time.Time
}

func newCachingResolver(next IdentityResolver) *cachingResolver {
	return &cachingResolver{next: next, m: map[string]cachedIdentity{}, now: time.Now}
}

func (c *cachingResolver) ResolveToken(ctx context.Context, token string) (*Identity, error) {
	sum := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(sum[:])
	now := c.now()
	c.mu.Lock()
	if e, ok := c.m[key]; ok && now.Before(e.exp) {
		c.mu.Unlock()
		id := e.id
		return &id, nil
	}
	c.mu.Unlock()
	id, err := c.next.ResolveToken(ctx, token)
	if err != nil || id == nil {
		return id, err
	}
	c.mu.Lock()
	if len(c.m) >= identityCacheMax {
		for k, e := range c.m {
			if !now.Before(e.exp) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= identityCacheMax {
			c.m = map[string]cachedIdentity{}
		}
	}
	c.m[key] = cachedIdentity{id: *id, exp: now.Add(identityCacheTTL)}
	c.mu.Unlock()
	return id, nil
}

// authLocalResolver resolves tokens through auth-local's AuthService
// (ExtractIdentity), dialing lazily on first use.
type authLocalResolver struct {
	mu     sync.Mutex
	conn   *grpc.ClientConn
	client authv1.AuthServiceClient
}

func (a *authLocalResolver) ResolveToken(ctx context.Context, token string) (*Identity, error) {
	cli, err := a.dial()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := cli.ExtractIdentity(ctx, &authv1.ExtractIdentityRequest{Token: token})
	if err != nil {
		return nil, fmt.Errorf("auth-local identity: %w", err)
	}
	if !resp.Found || strings.TrimSpace(resp.Id) == "" {
		return nil, nil
	}
	return &Identity{ID: resp.Id, Roles: resp.Roles}, nil
}

func (a *authLocalResolver) dial() (authv1.AuthServiceClient, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil {
		return a.client, nil
	}
	addr := strings.TrimSpace(os.Getenv("AUTH_LOCAL_GRPC_ADDR"))
	if addr == "" {
		addr = "localhost:9403"
	}
	var opt grpc.DialOption
	host, _, herr := net.SplitHostPort(addr)
	if herr != nil {
		host = addr
	}
	local := host == "localhost" || host == "127.0.0.1" || host == "::1" || host == ""
	if (os.Getenv("MUXCORE_INSECURE_DISABLE_TLS") == "true" || os.Getenv("MUXCORE_GRPC_INSECURE") == "true") && local {
		opt = grpc.WithTransportCredentials(insecure.NewCredentials())
	} else {
		creds, err := clientTLS()
		if err != nil {
			return nil, err
		}
		opt = grpc.WithTransportCredentials(creds)
	}
	conn, err := grpc.NewClient(addr, opt)
	if err != nil {
		return nil, fmt.Errorf("dial auth-local %s: %w", addr, err)
	}
	a.conn = conn
	a.client = authv1.NewAuthServiceClient(conn)
	slog.Info("jellyfin: identity resolution wired to auth-local", "addr", addr)
	return a.client, nil
}

func clientTLS() (credentials.TransportCredentials, error) {
	certFile := strings.TrimSpace(os.Getenv("MUXCORE_TLS_CERT"))
	keyFile := strings.TrimSpace(os.Getenv("MUXCORE_TLS_KEY"))
	if certFile == "" || keyFile == "" {
		return nil, errors.New("TLS required for auth-local dial: set MUXCORE_TLS_CERT/MUXCORE_TLS_KEY")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client TLS cert/key: %w", err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if ca := strings.TrimSpace(os.Getenv("MUXCORE_TLS_CA")); ca != "" {
		pemBytes, err := os.ReadFile(ca)
		if err != nil {
			return nil, fmt.Errorf("read TLS CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("parse TLS CA from %q", ca)
		}
		cfg.RootCAs = pool
	}
	return credentials.NewTLS(cfg), nil
}

// authenticateUser resolves the bearer token on r to the end user. On failure
// it returns the HTTP status and message to send.
func (m *Module) authenticateUser(r *http.Request) (*Identity, int, string) {
	token := bearerToken(r)
	if token == "" {
		return nil, http.StatusUnauthorized, "missing bearer token"
	}
	m.mu.RLock()
	res := m.identity
	m.mu.RUnlock()
	if res == nil {
		return nil, http.StatusServiceUnavailable, "identity provider not configured"
	}
	id, err := res.ResolveToken(r.Context(), token)
	if err != nil {
		slog.Warn("jellyfin: identity resolution failed", "error", err)
		return nil, http.StatusServiceUnavailable, "identity provider unavailable"
	}
	if id == nil || strings.TrimSpace(id.ID) == "" {
		return nil, http.StatusUnauthorized, errNoIdentity.Error()
	}
	return id, 0, ""
}
