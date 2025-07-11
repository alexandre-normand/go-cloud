// Copyright 2018 The Go Cloud Development Kit Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package cloudsql contains Wire providers that are common across Google Cloud
// SQL.
package cloudsql // import "github.com/alexandre-normand/go-cloud/gcp/cloudsql"

import (
	"github.com/GoogleCloudPlatform/cloudsql-proxy/proxy/certs"
	"github.com/GoogleCloudPlatform/cloudsql-proxy/proxy/proxy"
	"github.com/google/wire"
	"github.com/alexandre-normand/go-cloud/gcp"
	"golang.org/x/oauth2"
)

// CertSourceSet is a Wire provider set that binds a Cloud SQL proxy
// certificate source from an GCP-authenticated HTTP client.
var CertSourceSet = wire.NewSet(
	NewCertSource,
	wire.Bind(new(proxy.CertSource), new(*certs.RemoteCertSource)))

// NewCertSource creates a local certificate source that uses the given
// HTTP client. The client is assumed to make authenticated requests.
func NewCertSource(c *gcp.HTTPClient) *certs.RemoteCertSource {
	return certs.NewCertSourceOpts(&c.Client, certs.RemoteOpts{})
}

// NewCertSourceWithIAM creates a local certificate source, including Token source for token information used in
// cert creation, that uses the given HTTP client. The client is assumed to make authenticated requests.
func NewCertSourceWithIAM(c *gcp.HTTPClient, t oauth2.TokenSource) *certs.RemoteCertSource {
	return certs.NewCertSourceOpts(&c.Client, certs.RemoteOpts{EnableIAMLogin: true, TokenSource: t})
}
