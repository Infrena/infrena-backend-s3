// Command infrena-backend-s3 stores infrena state in an S3-compatible object
// store. Infrena runs it; you do not.
package main

import (
	"github.com/infrena/infrena-backend-s3/internal/s3backend"
	"github.com/infrena/infrena/pkg/backendsdk"
)

func main() { backendsdk.Main(s3backend.New()) }
