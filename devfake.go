//go:build devfake

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"s4browser/internal/browse"
	"s4browser/internal/fakes3"
	"s4browser/internal/queue"
)

// Development only: `wails dev -tags devfake` points browsing and transfers at an
// in-process fake S3 with fake keys, whatever profile is selected, so the UI can
// be tried with 100,000 objects and no AWS account. Release builds do not
// contain it.

const devProfileID = "dev:fake"

func devProfile(id string) string {
	if id == "" {
		return devProfileID
	}
	return id
}

// The one fake S3 that browsing and transfers share, so a transfer started in the
// UI shows up in the listing.
var (
	devOnce   sync.Once
	devServer *fakes3.Server
	devErr    error
)

func devFake() (*fakes3.Server, error) {
	devOnce.Do(func() { devServer, devErr = newDevFake() })
	return devServer, devErr
}

func newDevFake() (*fakes3.Server, error) {
	fake, err := fakes3.New(fakes3.Options{BucketRegions: map[string]string{
		"dev-media":  "us-west-2",
		"dev-small":  "us-east-1",
		"dev-eu":     "eu-west-1",
		"dev-hidden": "ap-southeast-2", // add it by name with S4_DEVFAKE_DENY_LIST=1
	}})
	if err != nil {
		return nil, err
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
	// Objects that can be downloaded: their content is a generated pattern. The
	// listed objects above have a size but no content, and fail on download.
	for i, size := range []int64{3 << 20, 200 << 20, 1 << 30, 8 << 30} {
		_ = fake.PutSynthetic("dev-small", fmt.Sprintf("downloads/sample%d.bin", i+1), uint64(i+1), size)
		_ = fake.PutSynthetic("dev-media", fmt.Sprintf("samples/sample%d.bin", i+1), uint64(i+11), size)
	}
	// Object commands: dev-media keeps versions, so a delete there warns about
	// delete markers; one sample carries headers, user metadata and tags.
	_ = fake.SetVersioning("dev-media", "Enabled")
	_ = fake.SetExtras("dev-small", "downloads/sample1.bin", fakes3.ObjectExtras{
		Headers: map[string]string{"Content-Type": "application/octet-stream", "Cache-Control": "max-age=3600"},
		Meta:    map[string]string{"camera": "A-cam", "shot-id": "042"},
		Tags:    []fakes3.Tag{{Key: "project", Value: "Demo"}, {Key: "reel", Value: "1"}},
	})
	// S4_DEVFAKE_DENY_LIST=1 emulates a role that may not list buckets, so
	// buckets must be added by name.
	if os.Getenv("S4_DEVFAKE_DENY_LIST") != "" {
		fake.AddRule(fakes3.Rule{Op: fakes3.OpListBuckets, Fault: fakes3.AccessDenied()})
	}
	log.Printf("devfake: fake S3 at %s", fake.Endpoint())
	return fake, nil
}

func devClientOptions(fake *fakes3.Server) func(*s3.Options) {
	return func(so *s3.Options) {
		so.BaseEndpoint = aws.String(fake.Endpoint())
		so.UsePathStyle = true
		so.HTTPClient = fake.HTTPClient()
	}
}

func devCredentials(string) (aws.CredentialsProvider, error) {
	return credentials.NewStaticCredentialsProvider(fakes3.AccessKeyID, fakes3.SecretAccessKey, ""), nil
}

func devBrowseOptions(o *browse.Options) error {
	fake, err := devFake()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "s4browser-devfake-")
	if err != nil {
		return err
	}
	log.Printf("devfake: saved buckets in %s", dir)
	o.Dir = dir
	o.Credentials = devCredentials
	o.ClientOptions = append(o.ClientOptions, devClientOptions(fake))
	return nil
}

// devQueueOptions routes transfers to the same fake. The journal and the
// transfer settings go to a temporary folder, so a dev run neither takes the real
// journal's lock nor changes the real settings. Credentials never expire.
func devQueueOptions(o *queue.Options) error {
	fake, err := devFake()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "s4browser-devfake-queue-")
	if err != nil {
		return err
	}
	log.Printf("devfake: transfer journal and settings in %s", dir)
	o.Dir = dir
	o.Provider = devCredentials
	o.Refresh = func(context.Context, string) error { return nil }
	o.Subscribe = nil
	o.ClientOptions = append(o.ClientOptions, devClientOptions(fake))
	return nil
}

// devPick answers a picker from the environment when the variable for it is
// set, so the UI can be driven by a script: S4_DEVFAKE_PICK_FILES (paths
// separated by "|"), S4_DEVFAKE_PICK_FOLDER and S4_DEVFAKE_PICK_DEST.
func devPick(kind pickKind) ([]string, bool) {
	name := map[pickKind]string{pickFiles: "S4_DEVFAKE_PICK_FILES", pickFolder: "S4_DEVFAKE_PICK_FOLDER", pickDest: "S4_DEVFAKE_PICK_DEST"}[kind]
	v := os.Getenv(name)
	if v == "" {
		return nil, false
	}
	return strings.Split(v, "|"), true
}
