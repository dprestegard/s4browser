//go:build devfake

package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"s4browser/internal/browse"
	"s4browser/internal/fakes3"
)

// Development only: `wails dev -tags devfake` points browsing at an in-process
// fake S3 with fake keys, whatever profile is selected, so the UI can be tried
// with 100,000 objects and no AWS account. Release builds do not contain it.

const devProfileID = "dev:fake"

func devProfile(id string) string {
	if id == "" {
		return devProfileID
	}
	return id
}

func devBrowseOptions(o *browse.Options) error {
	fake, err := fakes3.New(fakes3.Options{BucketRegions: map[string]string{
		"dev-media":  "us-west-2",
		"dev-small":  "us-east-1",
		"dev-eu":     "eu-west-1",
		"dev-hidden": "ap-southeast-2", // add it by name with S4_DEVFAKE_DENY_LIST=1
	}})
	if err != nil {
		return err
	}
	base := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	for i := range 100_000 {
		size := int64((i*7919)%100003) << 20
		mod := base.Add(time.Duration((i*104729)%1000003) * time.Second)
		_ = fake.PutListed("dev-media", fmt.Sprintf("big/shot%06d.exr", i), size, mod)
	}
	for i := range 40 {
		_ = fake.PutListed("dev-media", fmt.Sprintf("big/reel%d/clip.mxf", i+1), 5<<30, base)
		_ = fake.PutListed("dev-media", fmt.Sprintf("projects/p%d/edit.prproj", i), 1<<20, base)
	}
	_ = fake.PutListed("dev-small", "readme.txt", 1234, base)
	_ = fake.PutListed("dev-small", "a b+c%d.mov", 99, base)
	_ = fake.PutListed("dev-hidden", "secret/x.bin", 1, base)
	// S4_DEVFAKE_DENY_LIST=1 emulates a role that may not list buckets, so
	// buckets must be added by name.
	if os.Getenv("S4_DEVFAKE_DENY_LIST") != "" {
		fake.AddRule(fakes3.Rule{Op: fakes3.OpListBuckets, Fault: fakes3.AccessDenied()})
	}
	dir, err := os.MkdirTemp("", "s4browser-devfake-")
	if err != nil {
		return err
	}
	log.Printf("devfake: fake S3 at %s, saved buckets in %s", fake.Endpoint(), dir)
	o.Dir = dir
	o.Credentials = func(string) (aws.CredentialsProvider, error) {
		return credentials.NewStaticCredentialsProvider(fakes3.AccessKeyID, fakes3.SecretAccessKey, ""), nil
	}
	o.ClientOptions = append(o.ClientOptions, func(so *s3.Options) {
		so.BaseEndpoint = aws.String(fake.Endpoint())
		so.UsePathStyle = true
		so.HTTPClient = fake.HTTPClient()
	})
	return nil
}
