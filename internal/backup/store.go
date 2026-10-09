package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/wes/jokku/internal/types"
)

// Store is where backups are kept: a bucket, as objects named by key.
type Store interface {
	// Check reports whether the bucket can be reached with these
	// credentials.
	Check(ctx context.Context) error
	Put(ctx context.Context, key string, data []byte) error
	// Get returns ErrNotFound for a key that isn't there.
	Get(ctx context.Context, key string) ([]byte, error)
	// List returns the keys under prefix, sorted.
	List(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, key string) error
}

// ErrNotFound is a key the store doesn't have.
var ErrNotFound = errors.New("not found")

// Open connects to a destination's bucket. Tests replace it.
var Open = OpenS3

// OpenS3 connects to an S3-compatible bucket: AWS, Tigris, R2, B2, MinIO.
func OpenS3(d types.BackupDestination) (Store, error) {
	u, err := ParseEndpoint(d.Endpoint)
	if err != nil {
		return nil, err
	}
	c, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(d.AccessKeyID, d.SecretAccessKey, ""),
		Secure:       u.Scheme == "https",
		Region:       d.Region,
		BucketLookup: minio.BucketLookupAuto,
	})
	if err != nil {
		return nil, err
	}
	return &s3Store{c: c, bucket: d.Bucket}, nil
}

// ParseEndpoint checks an endpoint is a bare https:// (or http://) host,
// adding https:// when no scheme is given.
func ParseEndpoint(endpoint string) (*url.URL, error) {
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || strings.Trim(u.Path, "/") != "" || u.RawQuery != "" {
		return nil, fmt.Errorf("%q is not an S3 endpoint, like https://fly.storage.tigris.dev or https://s3.us-east-1.amazonaws.com", endpoint)
	}
	return u, nil
}

type s3Store struct {
	c      *minio.Client
	bucket string
}

func (s *s3Store) Check(ctx context.Context) error {
	ok, err := s.c.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("there is no bucket %s", s.bucket)
	}
	return nil
}

func (s *s3Store) Put(ctx context.Context, key string, data []byte) error {
	_, err := s.c.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	return err
}

func (s *s3Store) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.c.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, s3Err(err)
	}
	defer obj.Close()
	b, err := io.ReadAll(obj)
	return b, s3Err(err)
}

func (s *s3Store) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	for obj := range s.c.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		keys = append(keys, obj.Key)
	}
	sort.Strings(keys)
	return keys, nil
}

func (s *s3Store) Delete(ctx context.Context, key string) error {
	return s.c.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

func s3Err(err error) error {
	if err != nil && minio.ToErrorResponse(err).Code == "NoSuchKey" {
		return ErrNotFound
	}
	return err
}

// Dir is a store in a local directory, for tests.
type Dir string

func (d Dir) Check(context.Context) error {
	_, err := os.Stat(string(d))
	return err
}

func (d Dir) path(key string) string { return filepath.Join(string(d), filepath.FromSlash(key)) }

func (d Dir) Put(_ context.Context, key string, data []byte) error {
	p := d.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (d Dir) Get(_ context.Context, key string) ([]byte, error) {
	b, err := os.ReadFile(d.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

func (d Dir) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	err := filepath.WalkDir(string(d), func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || strings.HasSuffix(p, ".tmp") {
			return err
		}
		rel, err := filepath.Rel(string(d), p)
		if key := filepath.ToSlash(rel); err == nil && strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return err
	})
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	sort.Strings(keys)
	return keys, err
}

func (d Dir) Delete(_ context.Context, key string) error {
	err := os.Remove(d.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
