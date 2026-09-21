package s3backend

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// defaultEndpoint is AWS S3, which is where a block with no `endpoint:` means.
const defaultEndpoint = "https://s3.amazonaws.com"

// defaultRegion is what a store that is not AWS is told when the block names
// no region. Nearly every S3-compatible store either ignores the region or
// expects this one, and a signature computed for the wrong region is refused
// with an error that says nothing useful about why.
const defaultRegion = "us-east-1"

// newClient builds the minio client this backend talks to.
//
// CREDENTIALS ARE NEVER CONFIGURATION. They resolve through a chain, in the
// order a user would expect to be able to override them: the named profile in
// the shared AWS credentials or config file, then the environment, then an
// instance role. `profile:` selects a profile, which is a name, and is the
// only credential-shaped thing the `backend:` block can say.
func newClient(c Config) (*minio.Client, error) {
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	// A scheme is required rather than guessed: "127.0.0.1:9000" is
	// ambiguous between http and https, and guessing https for a local MinIO
	// fails with a TLS handshake error that reads like a network problem.
	if !strings.Contains(endpoint, "://") {
		return nil, fmt.Errorf("`backend.endpoint` must start with http:// or https://, but it is %q", endpoint)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("`backend.endpoint` is not a URL: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("`backend.endpoint` names no host: %q", endpoint)
	}

	region := c.Region
	if region == "" && c.Endpoint != "" {
		region = defaultRegion
	}

	lookup := minio.BucketLookupDNS
	if c.pathStyle() {
		lookup = minio.BucketLookupPath
	}

	return minio.New(u.Host, &minio.Options{
		Creds: credentials.NewChainCredentials([]credentials.Provider{
			&credentials.FileAWSCredentials{Profile: c.Profile},
			&credentials.EnvAWS{},
			&credentials.IAM{},
		}),
		Secure:       u.Scheme == "https",
		Region:       region,
		BucketLookup: lookup,
	})
}
