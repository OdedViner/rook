/*
Copyright 2018 The Rook Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package object

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/url"
	"strings"

	v2aws "github.com/aws/aws-sdk-go-v2/aws"
	v2creds "github.com/aws/aws-sdk-go-v2/credentials"
	s3v2 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	awssession "github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	endpoints "github.com/aws/smithy-go/endpoints"
	"github.com/pkg/errors"
)

// Region for aws golang sdk
const CephRegion = "us-east-1"

type staticS3Resolver struct {
	u *url.URL
}

func (r staticS3Resolver) ResolveEndpoint(ctx context.Context, params s3v2.EndpointParameters) (endpoints.Endpoint, error) {
	// smithy endpoints.Endpoint expects a url.URL, not a string
	return endpoints.Endpoint{
		URI: *r.u,
	}, nil
}

// S3Agent wraps the s3.S3 structure to allow for wrapper methods
type S3Agent struct {
	Client   *s3.S3       // v1 client
	ClientV2 *s3v2.Client // v2 client
}

func NewS3Agent(accessKey, secretKey, endpoint string, debug bool, tlsCert []byte, insecure bool, httpClient *http.Client) (*S3Agent, error) {
	logLevel := aws.LogOff
	if debug {
		logLevel = aws.LogDebug
	}

	tlsEnabled := false
	if len(tlsCert) > 0 || insecure {
		tlsEnabled = true
	}
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: HttpTimeOut,
		}
		if tlsEnabled {
			httpClient.Transport = BuildTransportTLS(tlsCert, insecure)
		}
	}

	// -----------------------------
	// SDK v1 client initialization
	// -----------------------------
	v1Session, err := awssession.NewSession(
		aws.NewConfig().
			WithRegion(CephRegion).
			WithCredentials(credentials.NewStaticCredentials(accessKey, secretKey, "")).
			WithEndpoint(endpoint).
			WithS3ForcePathStyle(true).
			WithMaxRetries(5).
			WithDisableSSL(!tlsEnabled).
			WithHTTPClient(httpClient).
			WithLogLevel(logLevel),
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create v1 session")
	}
	v1Client := s3.New(v1Session)

	// -----------------------------
	// SDK v2 client initialization
	// -----------------------------
	scheme := "http"
	if tlsEnabled {
		scheme = "https"
	}
	u, perr := url.Parse(endpoint)
	if perr != nil || u.Scheme == "" {
		u, _ = url.Parse(scheme + "://" + endpoint)
	}
	v2Cfg := v2aws.Config{
		Region:      CephRegion,
		Credentials: v2aws.NewCredentialsCache(v2creds.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		HTTPClient:  httpClient,
	}
	v2Client := s3v2.NewFromConfig(v2Cfg, func(o *s3v2.Options) {
		o.UsePathStyle = true
		o.EndpointResolverV2 = staticS3Resolver{u: u}
	})
	return &S3Agent{
		Client:   v1Client,
		ClientV2: v2Client,
	}, nil
}

// CreateBucket creates a bucket with the given name
func (s *S3Agent) CreateBucketNoInfoLogging(name string) error {
	return s.createBucket(name, false)
}

// CreateBucket creates a bucket with the given name
func (s *S3Agent) CreateBucket(name string) error {
	return s.createBucket(name, true)
}

func (s *S3Agent) createBucket(name string, infoLogging bool) error {
	if infoLogging {
		logger.Infof("creating bucket %q", name)
	} else {
		logger.Debugf("creating bucket %q", name)
	}

	input := &s3v2.CreateBucketInput{
		Bucket: &name,
	}

	_, err := s.ClientV2.CreateBucket(context.TODO(), input)
	if err != nil {
		var alreadyExists *s3types.BucketAlreadyExists
		var alreadyOwned *s3types.BucketAlreadyOwnedByYou
		if errors.As(err, &alreadyExists) || errors.As(err, &alreadyOwned) {
			logger.Debugf("bucket %q already exists or is owned by you", name)
			return nil
		}
		return errors.Wrapf(err, "failed to create bucket %q", name)
	}

	if infoLogging {
		logger.Infof("successfully created bucket %q", name)
	} else {
		logger.Debugf("successfully created bucket %q", name)
	}
	return nil
}

// DeleteBucket function deletes given bucket using s3 client
func (s *S3Agent) DeleteBucket(name string) (bool, error) {
	_, err := s.Client.DeleteBucket(&s3.DeleteBucketInput{
		Bucket: aws.String(name),
	})
	if err != nil {
		logger.Errorf("failed to delete bucket. %v", err)
		return false, err

	}
	return true, nil
}

// PutObjectInBucket function puts an object in a bucket using s3 client
func (s *S3Agent) PutObjectInBucket(bucketname string, body string, key string,
	contentType string,
) (bool, error) {
	_, err := s.Client.PutObject(&s3.PutObjectInput{
		Body:        strings.NewReader(body),
		Bucket:      &bucketname,
		Key:         &key,
		ContentType: &contentType,
	})
	if err != nil {
		logger.Errorf("failed to put object in bucket. %v", err)
		return false, err

	}
	return true, nil
}

// GetObjectInBucket function retrieves an object from a bucket using s3 client
func (s *S3Agent) GetObjectInBucket(bucketname string, key string) (string, error) {
	result, err := s.Client.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(bucketname),
		Key:    aws.String(key),
	})
	if err != nil {
		logger.Errorf("failed to retrieve object from bucket. %v", err)
		return "ERROR_ OBJECT NOT FOUND", err

	}
	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(result.Body)
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

// DeleteObjectInBucket function deletes given bucket using s3 client
func (s *S3Agent) DeleteObjectInBucket(bucketname string, key string) (bool, error) {
	_, err := s.Client.DeleteObject(&s3.DeleteObjectInput{
		Bucket: aws.String(bucketname),
		Key:    aws.String(key),
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case s3.ErrCodeNoSuchBucket:
				return true, nil
			case s3.ErrCodeNoSuchKey:
				return true, nil
			}
		}
		logger.Errorf("failed to delete object from bucket. %v", err)
		return false, err

	}
	return true, nil
}

func BuildTransportTLS(tlsCert []byte, insecure bool) *http.Transport {
	//nolint:gosec // is enabled only for testing
	tlsConfig := &tls.Config{InsecureSkipVerify: insecure}
	var caCertPool *x509.CertPool
	var err error
	caCertPool, err = x509.SystemCertPool()
	if err != nil {
		logger.Warningf("failed to load system cert pool; continuing without loading system certs")
		caCertPool = x509.NewCertPool() // start with empty cert pool instead
	}
	if len(tlsCert) > 0 {
		caCertPool.AppendCertsFromPEM(tlsCert)
	}
	tlsConfig.RootCAs = caCertPool

	return &http.Transport{
		TLSClientConfig: tlsConfig,
	}
}
