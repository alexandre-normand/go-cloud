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

// Package awscloud contains Wire providers for AWS services.
package awscloud // import "github.com/alexandre-normand/go-cloud/aws/awscloud"

import (
	"net/http"

	"github.com/google/wire"
	"github.com/alexandre-normand/go-cloud/aws"
	"github.com/alexandre-normand/go-cloud/aws/rds"
	"github.com/alexandre-normand/go-cloud/blob/s3blob"
	"github.com/alexandre-normand/go-cloud/docstore/awsdynamodb"
	"github.com/alexandre-normand/go-cloud/pubsub/awssnssqs"
	"github.com/alexandre-normand/go-cloud/runtimevar/awsparamstore"
	"github.com/alexandre-normand/go-cloud/secrets/awskms"
	"github.com/alexandre-normand/go-cloud/server/xrayserver"
)

// AWS is a Wire provider set that includes all Amazon Web Services interface
// implementations in the Go CDK and authenticates using the default session.
var AWS = wire.NewSet(
	Services,
	aws.DefaultSession,
	aws.NewDefaultV2Config,
	wire.Value(http.DefaultClient),
)

// Services is a Wire provider set that includes the default wiring for all
// Amazon Web Services interface implementations in the Go CDK but unlike the
// AWS set, does not include credentials. Individual services may require
// additional configuration.
var Services = wire.NewSet(
	s3blob.Set,
	awssnssqs.Set,
	awsparamstore.Set,
	awskms.Set,
	rds.CertFetcherSet,
	awsdynamodb.Set,
	xrayserver.Set,
)
