//go:build ignore

/*******************************************************************************
 * Copyright (c) 2026 Genome Research Ltd.
 *
 * Author: Sendu Bala <sb10@sanger.ac.uk>
 *
 * Permission is hereby granted, free of charge, to any person obtaining
 * a copy of this software and associated documentation files (the
 * "Software"), to deal in the Software without restriction, including
 * without limitation the rights to use, copy, modify, merge, publish,
 * distribute, sublicense, and/or sell copies of the Software, and to
 * permit persons to whom the Software is furnished to do so, subject to
 * the following conditions:
 *
 * The above copyright notice and this permission notice shall be included
 * in all copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
 * EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
 * MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.
 * IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY
 * CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT,
 * TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE
 * SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
 ******************************************************************************/

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"

	muxfys "github.com/VertebrateResequencing/muxfys/v4"
	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const (
	defaultBucket = "sb10-muxfys-testing"
	defaultPrefix = "wr_tests"
)

type zeroReader struct {
	remaining int64
}

type targetParts struct {
	secure bool
	host   string
	bucket string
	prefix string
}

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.remaining == 0 {
		return 0, io.EOF
	}

	if int64(len(p)) > z.remaining {
		p = p[:int(z.remaining)]
	}

	for i := range p {
		p[i] = 0
	}

	z.remaining -= int64(len(p))

	return len(p), nil
}

func main() {
	bucket := flag.String("bucket", defaultBucket, "bucket to seed")
	prefix := flag.String("prefix", defaultPrefix, "bucket prefix to reset and seed")
	target := flag.String("target", "", "full target URL; defaults to ~/.s3cfg-derived bucket/prefix URL")
	bigSize := flag.Int64("big-size", 1073741824, "size of big.file in bytes")
	clean := flag.Bool("clean", true, "delete existing objects under the prefix before seeding")
	flag.Parse()

	cfg, err := muxfys.S3ConfigFromEnvironment("", path.Join(*bucket, *prefix))
	if err != nil {
		fatalf("read S3 config: %v", err)
	}
	if *target != "" {
		cfg.Target = *target
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		fatalf("write credentials are required; set them in ~/.s3cfg or AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY")
	}

	parts, err := parseTarget(cfg.Target)
	if err != nil {
		fatalf("parse target: %v", err)
	}
	if parts.prefix == "" || parts.prefix == "." || parts.prefix == "/" {
		fatalf("refusing to seed an empty prefix")
	}

	client, err := minio.New(parts.host, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Region: cfg.Region,
		Secure: parts.secure,
	})
	if err != nil {
		fatalf("create S3 client: %v", err)
	}

	ctx := context.Background()
	if err := ensureBucket(ctx, client, parts.bucket); err != nil {
		fatalf("ensure bucket: %v", err)
	}
	if *clean {
		if err := cleanPrefix(ctx, client, parts.bucket, parts.prefix); err != nil {
			fatalf("clean prefix: %v", err)
		}
	}
	if err := seed(ctx, client, parts.bucket, parts.prefix, *bigSize); err != nil {
		fatalf("seed fixtures: %v", err)
	}

	fmt.Printf("Seeded %s/%s with remote S3 test fixtures\n", parts.bucket, parts.prefix)
}

func parseTarget(target string) (*targetParts, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}

	pathParts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(pathParts) < 2 {
		return nil, fmt.Errorf("target must include bucket and prefix: %s", target)
	}

	return &targetParts{
		secure: u.Scheme == "https",
		host:   u.Host,
		bucket: pathParts[0],
		prefix: path.Join(pathParts[1:]...),
	}, nil
}

func ensureBucket(ctx context.Context, client *minio.Client, bucket string) error {
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	return client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{})
}

func cleanPrefix(ctx context.Context, client *minio.Client, bucket, prefix string) error {
	objectCh := client.ListObjects(ctx, bucket, minio.ListObjectsOptions{
		Prefix:    prefix + "/",
		Recursive: true,
	})
	for object := range objectCh {
		if object.Err != nil {
			return object.Err
		}
		if err := client.RemoveObject(ctx, bucket, object.Key, minio.RemoveObjectOptions{}); err != nil {
			return err
		}
	}

	return nil
}

func seed(ctx context.Context, client *minio.Client, bucket, prefix string, bigSize int64) error {
	if err := putString(ctx, client, bucket, path.Join(prefix, "100k.lines"), hundredThousandLines()); err != nil {
		return err
	}
	if err := putString(ctx, client, bucket, path.Join(prefix, "numalphanum.txt"), "1234567890abcdefghijklmnopqrstuvwxyz1234567890\n"); err != nil {
		return err
	}
	if err := putString(ctx, client, bucket, path.Join(prefix, "sub/empty.file"), ""); err != nil {
		return err
	}
	if err := putString(ctx, client, bucket, path.Join(prefix, "sub/deep/bar"), "foo\n"); err != nil {
		return err
	}
	if err := putReader(ctx, client, bucket, path.Join(prefix, "big.file"), &zeroReader{remaining: bigSize}, bigSize); err != nil {
		return err
	}

	return putReader(ctx, client, bucket, prefix+"/emptyDir/", bytes.NewReader(nil), 0)
}

func hundredThousandLines() string {
	var lines strings.Builder
	for i := 1; i <= 100000; i++ {
		fmt.Fprintf(&lines, "%06d\n", i)
	}

	return lines.String()
}

func putString(ctx context.Context, client *minio.Client, bucket, object, content string) error {
	reader := strings.NewReader(content)

	return putReader(ctx, client, bucket, object, reader, int64(reader.Len()))
}

func putReader(ctx context.Context, client *minio.Client, bucket, object string, reader io.Reader, size int64) error {
	_, err := client.PutObject(ctx, bucket, object, reader, size, minio.PutObjectOptions{})
	if err != nil {
		return fmt.Errorf("put %s: %w", object, err)
	}

	return nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
