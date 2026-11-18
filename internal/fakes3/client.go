package fakes3

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Client returns a real aws-sdk-go-v2 S3 client for the server, with
// path-style addressing, the server's certificate trusted and the fake keys.
// Tests pass s3x.SingleAttemptOptions on each call, as the engine does.
func (s *Server) S3Client(opts ...func(*s3.Options)) *s3.Client {
	base := func(o *s3.Options) {
		o.Region = Region
		o.BaseEndpoint = aws.String(s.url)
		o.UsePathStyle = true
		o.HTTPClient = s.client
		o.Credentials = credentials.NewStaticCredentialsProvider(AccessKeyID, SecretAccessKey, "")
	}
	return s3.New(s3.Options{}, append([]func(*s3.Options){base}, opts...)...)
}
