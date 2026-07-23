package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"go.uber.org/zap"
)

// ecrAuthRefreshBuffer is how long before a token's real expiry we proactively
// refresh, so a pull never races the expiry boundary.
const ecrAuthRefreshBuffer = 30 * time.Minute

// ecrAuthProvider fetches and caches an ECR X-Registry-Auth token via the AWS
// SDK (IRSA on EKS — no static secret, no CronJob). The token is refreshed
// automatically before it expires, so pulls never fail with
// "authorization token has expired".
type ecrAuthProvider struct {
	registry string
	region   string
	logger   *zap.Logger

	mu        sync.Mutex
	client    *ecr.Client
	cached    string
	expiresAt time.Time
}

// ecrRegion parses the AWS region from an ECR registry host of the form
// <acct>.dkr.ecr.<region>.amazonaws.com[.cn]. Returns ("", false) if the host
// is not an ECR registry.
func ecrRegion(registry string) (string, bool) {
	host := strings.TrimRight(registry, "/")
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	parts := strings.Split(host, ".")
	if len(parts) >= 6 && parts[1] == "dkr" && parts[2] == "ecr" && parts[4] == "amazonaws" {
		return parts[3], true
	}
	return "", false
}

// newECRAuthProvider returns a provider when registry is an ECR host, else nil
// (callers then fall back to any static X-Registry-Auth).
func newECRAuthProvider(registry string, logger *zap.Logger) *ecrAuthProvider {
	region, ok := ecrRegion(registry)
	if !ok {
		return nil
	}
	return &ecrAuthProvider{
		registry: strings.TrimRight(registry, "/"),
		region:   region,
		logger:   logger,
	}
}

// Get returns a valid base64 X-Registry-Auth, refreshing via ECR when the
// cached value is missing or within ecrAuthRefreshBuffer of expiry.
func (p *ecrAuthProvider) Get(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cached != "" && time.Now().Before(p.expiresAt.Add(-ecrAuthRefreshBuffer)) {
		return p.cached, nil
	}

	if p.client == nil {
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(p.region))
		if err != nil {
			return "", fmt.Errorf("ecr_auth: load aws config: %w", err)
		}
		p.client = ecr.NewFromConfig(awsCfg)
	}

	out, err := p.client.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return "", fmt.Errorf("ecr_auth: get authorization token: %w", err)
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return "", fmt.Errorf("ecr_auth: empty authorization data")
	}
	ad := out.AuthorizationData[0]

	// AuthorizationToken is base64("AWS:<password>").
	decoded, err := base64.StdEncoding.DecodeString(*ad.AuthorizationToken)
	if err != nil {
		return "", fmt.Errorf("ecr_auth: decode token: %w", err)
	}
	user, pass, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return "", fmt.Errorf("ecr_auth: malformed authorization token")
	}

	authJSON, err := json.Marshal(map[string]string{
		"username":      user,
		"password":      pass,
		"serveraddress": p.registry,
	})
	if err != nil {
		return "", fmt.Errorf("ecr_auth: marshal auth: %w", err)
	}
	p.cached = base64.StdEncoding.EncodeToString(authJSON)
	if ad.ExpiresAt != nil {
		p.expiresAt = *ad.ExpiresAt
	} else {
		p.expiresAt = time.Now().Add(12 * time.Hour)
	}
	p.logger.Info("refreshed ECR auth token via IRSA", zap.Time("expires_at", p.expiresAt))
	return p.cached, nil
}
