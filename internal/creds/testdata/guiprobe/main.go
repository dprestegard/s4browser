// Command guiprobe is built by the creds tests as a GUI-subsystem program, so a
// credential helper starts the way it does from the real app. It writes the
// access key ID it received to the result file.
package main

import (
	"context"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"

	"s4browser/internal/creds"
)

func main() {
	mode, command, result := os.Args[1], os.Args[2], os.Args[3]
	var provider aws.CredentialsProvider = creds.NewProcessProvider(command)
	if mode == "sdk" {
		provider = processcreds.NewProvider(command)
	}
	c, err := provider.Retrieve(context.Background())
	if err != nil {
		_ = os.WriteFile(result, []byte(err.Error()), 0o600)
		os.Exit(1)
	}
	if err := os.WriteFile(result, []byte(c.AccessKeyID), 0o600); err != nil {
		os.Exit(1)
	}
}
