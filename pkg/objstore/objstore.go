// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package objstore

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/thanos-io/thanos/pkg/runutil"
)

// Bucket provides read and write access to an object storage bucket.
// NOTE: We assume strong consistency for write-read flow.
type Bucket interface {
	io.Closer
	BucketReader

	// Upload the contents of the reader as an object into the bucket.
	// Upload should be idempotent.
	Upload(ctx context.Context, name string, r io.Reader) error

	// Delete removes the object with the given name.
	// If object does not exists in the moment of deletion, Delete should throw error.
	Delete(ctx context.Context, name string) error

	// Name returns the bucket name for the provider.
	Name() string
}

// BucketReader provides read access to an object storage bucket.
type BucketReader interface {
	// Iter calls f for each entry in the given directory (not recursive.). The argument to f is the full
	// object name including the prefix of the inspected directory.
	Iter(ctx context.Context, dir string, f func(string) error) error

	// Get returns a reader for the given object name.
	Get(ctx context.Context, name string) (io.ReadCloser, error)

	// GetRange returns a new range reader for the given object name and range.
	GetRange(ctx context.Context, name string, off, length int64) (io.ReadCloser, error)

	// Exists checks if the given object exists in the bucket.
	// TODO(bplotka): Consider removing Exists in favor of helper that do Get & IsObjNotFoundErr (less code to maintain).
	Exists(ctx context.Context, name string) (bool, error)

	// IsObjNotFoundErr returns true if error means that object is not found. Relevant to Get operations.
	IsObjNotFoundErr(err error) bool

	// ObjectSize returns the size of the specified object.
	ObjectSize(ctx context.Context, name string) (uint64, error)
}

// UploadDir uploads all files in srcdir to the bucket with into a top-level directory
// named dstdir. It is a caller responsibility to clean partial upload in case of failure.
func UploadDir(ctx context.Context, logger log.Logger, bkt Bucket, srcdir, dstdir string) error {
	df, err := os.Stat(srcdir)
	if err != nil {
		return errors.Wrap(err, "stat dir")
	}
	if !df.IsDir() {
		return errors.Errorf("%s is not a directory", srcdir)
	}
	return filepath.Walk(srcdir, func(src string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		dst := filepath.Join(dstdir, strings.TrimPrefix(src, srcdir))

		return UploadFile(ctx, logger, bkt, src, dst)
	})
}

// UploadFile uploads the file with the given name to the bucket.
// It is a caller responsibility to clean partial upload in case of failure.
func UploadFile(ctx context.Context, logger log.Logger, bkt Bucket, src, dst string) error {
	r, err := os.Open(src)
	if err != nil {
		return errors.Wrapf(err, "open file %s", src)
	}
	defer runutil.CloseWithLogOnErr(logger, r, "close file %s", src)

	if err := bkt.Upload(ctx, dst, r); err != nil {
		return errors.Wrapf(err, "upload file %s as %s", src, dst)
	}
	level.Debug(logger).Log("msg", "uploaded file", "from", src, "dst", dst, "bucket", bkt.Name())
	return nil
}

// DirDelim is the delimiter used to model a directory structure in an object store bucket.
const DirDelim = "/"

// DownloadFile downloads the src file from the bucket to dst. If dst is an existing
// directory, a file with the same name as the source is created in dst.
// If destination file is already existing, download file will overwrite it.
func DownloadFile(ctx context.Context, logger log.Logger, bkt BucketReader, src, dst string) (err error) {
	if fi, err := os.Stat(dst); err == nil {
		if fi.IsDir() {
			dst = filepath.Join(dst, filepath.Base(src))
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	rc, err := bkt.Get(ctx, src)
	if err != nil {
		return errors.Wrapf(err, "get file %s", src)
	}
	defer runutil.CloseWithLogOnErr(logger, rc, "download block's file reader")

	f, err := os.Create(dst)
	if err != nil {
		return errors.Wrap(err, "create file")
	}
	defer func() {
		if err != nil {
			if rerr := os.Remove(dst); rerr != nil {
				level.Warn(logger).Log("msg", "failed to remove partially downloaded file", "file", dst, "err", rerr)
			}
		}
	}()
	defer runutil.CloseWithLogOnErr(logger, f, "download block's output file")

	if _, err = io.Copy(f, rc); err != nil {
		return errors.Wrap(err, "copy object to file")
	}
	return nil
}

// DownloadDir downloads all object found in the directory into the local directory.
func DownloadDir(ctx context.Context, logger log.Logger, bkt BucketReader, src, dst string) error {
	if err := os.MkdirAll(dst, 0777); err != nil {
		return errors.Wrap(err, "create dir")
	}

	var downloadedFiles []string
	if err := bkt.Iter(ctx, src, func(name string) error {
		if strings.HasSuffix(name, DirDelim) {
			return DownloadDir(ctx, logger, bkt, name, filepath.Join(dst, filepath.Base(name)))
		}
		if err := DownloadFile(ctx, logger, bkt, name, dst); err != nil {
			return err
		}

		downloadedFiles = append(downloadedFiles, dst)
		return nil
	}); err != nil {
		// Best-effort cleanup if the download failed.
		for _, f := range downloadedFiles {
			if rerr := os.Remove(f); rerr != nil {
				level.Warn(logger).Log("msg", "failed to remove file on partial dir download error", "file", f, "err", rerr)
			}
		}
		return err
	}

	return nil
}

// Exists returns true, if file exists, otherwise false and nil error if presence IsObjNotFoundErr, otherwise false with
// returning error.
func Exists(ctx context.Context, bkt Bucket, src string) (bool, error) {
	rc, err := bkt.Get(ctx, src)
	if rc != nil {
		_ = rc.Close()
	}
	if err != nil {
		if bkt.IsObjNotFoundErr(err) {
			return false, nil
		}
		return false, errors.Wrap(err, "stat object")
	}

	return true, nil
}

const (
	iterOp     = "iter"
	sizeOp     = "objectsize"
	getOp      = "get"
	getRangeOp = "get_range"
	existsOp   = "exists"
	uploadOp   = "upload"
	deleteOp   = "delete"
)

// BucketWithMetrics takes a bucket and registers metrics with the given registry for
// operations run against the bucket.
func BucketWithMetrics(name string, b Bucket, reg prometheus.Registerer) Bucket {
	bkt := &metricBucket{
		bkt: b,

		ops: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name:        "thanos_objstore_bucket_operations_total",
			Help:        "Total number of operations against a bucket.",
			ConstLabels: prometheus.Labels{"bucket": name},
		}, []string{"operation"}),

		opsFailures: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name:        "thanos_objstore_bucket_operation_failures_total",
			Help:        "Total number of operations against a bucket that failed.",
			ConstLabels: prometheus.Labels{"bucket": name},
		}, []string{"operation"}),

		opsDuration: promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
			Name:        "thanos_objstore_bucket_operation_duration_seconds",
			Help:        "Duration of operations against the bucket",
			ConstLabels: prometheus.Labels{"bucket": name},
			Buckets:     []float64{0.001, 0.01, 0.1, 0.3, 0.6, 1, 3, 6, 9, 20, 30, 60, 90, 120},
		}, []string{"operation"}),
		lastSuccessfulUploadTime: promauto.With(reg).NewGaugeVec(prometheus.GaugeOpts{
			Name: "thanos_objstore_bucket_last_successful_upload_time",
			Help: "Second timestamp of the last successful upload to the bucket.",
		}, []string{"bucket"}),
	}
	for _, op := range []string{iterOp, sizeOp, getOp, getRangeOp, existsOp, uploadOp, deleteOp} {
		bkt.ops.WithLabelValues(op)
		bkt.opsFailures.WithLabelValues(op)
		bkt.opsDuration.WithLabelValues(op)
	}
	bkt.lastSuccessfulUploadTime.WithLabelValues(b.Name())
	return bkt
}

type metricBucket struct {
	bkt Bucket

	ops                      *prometheus.CounterVec
	opsFailures              *prometheus.CounterVec
	opsDuration              *prometheus.HistogramVec
	lastSuccessfulUploadTime *prometheus.GaugeVec
}

func (b *metricBucket) Iter(ctx context.Context, dir string, f func(name string) error) error {
	err := b.bkt.Iter(ctx, dir, f)
	if err != nil {
		b.opsFailures.WithLabelValues(iterOp).Inc()
	}
	b.ops.WithLabelValues(iterOp).Inc()

	return err
}

// ObjectSize returns the size of the specified object.
func (b *metricBucket) ObjectSize(ctx context.Context, name string) (uint64, error) {
	b.ops.WithLabelValues(sizeOp).Inc()
	start := time.Now()

	rc, err := b.bkt.ObjectSize(ctx, name)
	if err != nil {
		b.opsFailures.WithLabelValues(sizeOp).Inc()
		return 0, err
	}
	b.opsDuration.WithLabelValues(sizeOp).Observe(time.Since(start).Seconds())
	return rc, nil
}

func (b *metricBucket) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	b.ops.WithLabelValues(getOp).Inc()

	rc, err := b.bkt.Get(ctx, name)
	if err != nil {
		b.opsFailures.WithLabelValues(getOp).Inc()
		return nil, err
	}
	return newTimingReadCloser(
		rc,
		getOp,
		b.opsDuration,
		b.opsFailures,
	), nil
}

func (b *metricBucket) GetRange(ctx context.Context, name string, off, length int64) (io.ReadCloser, error) {
	b.ops.WithLabelValues(getRangeOp).Inc()

	rc, err := b.bkt.GetRange(ctx, name, off, length)
	if err != nil {
		b.opsFailures.WithLabelValues(getRangeOp).Inc()
		return nil, err
	}
	return newTimingReadCloser(
		rc,
		getRangeOp,
		b.opsDuration,
		b.opsFailures,
	), nil
}

func (b *metricBucket) Exists(ctx context.Context, name string) (bool, error) {
	start := time.Now()

	ok, err := b.bkt.Exists(ctx, name)
	if err != nil {
		b.opsFailures.WithLabelValues(existsOp).Inc()
	}
	b.ops.WithLabelValues(existsOp).Inc()
	b.opsDuration.WithLabelValues(existsOp).Observe(time.Since(start).Seconds())

	return ok, err
}

func (b *metricBucket) Upload(ctx context.Context, name string, r io.Reader) error {
	start := time.Now()

	err := b.bkt.Upload(ctx, name, r)
	if err != nil {
		b.opsFailures.WithLabelValues(uploadOp).Inc()
	} else {
		b.lastSuccessfulUploadTime.WithLabelValues(b.bkt.Name()).SetToCurrentTime()
	}
	b.ops.WithLabelValues(uploadOp).Inc()
	b.opsDuration.WithLabelValues(uploadOp).Observe(time.Since(start).Seconds())

	return err
}

func (b *metricBucket) Delete(ctx context.Context, name string) error {
	start := time.Now()

	err := b.bkt.Delete(ctx, name)
	if err != nil {
		b.opsFailures.WithLabelValues(deleteOp).Inc()
	}
	b.ops.WithLabelValues(deleteOp).Inc()
	b.opsDuration.WithLabelValues(deleteOp).Observe(time.Since(start).Seconds())

	return err
}

func (b *metricBucket) IsObjNotFoundErr(err error) bool {
	return b.bkt.IsObjNotFoundErr(err)
}

func (b *metricBucket) Close() error {
	return b.bkt.Close()
}

func (b *metricBucket) Name() string {
	return b.bkt.Name()
}

type timingReadCloser struct {
	io.ReadCloser

	ok       bool
	start    time.Time
	op       string
	duration *prometheus.HistogramVec
	failed   *prometheus.CounterVec
}

func newTimingReadCloser(rc io.ReadCloser, op string, dur *prometheus.HistogramVec, failed *prometheus.CounterVec) *timingReadCloser {
	// Initialize the metrics with 0.
	dur.WithLabelValues(op)
	failed.WithLabelValues(op)
	return &timingReadCloser{
		ReadCloser: rc,
		ok:         true,
		start:      time.Now(),
		op:         op,
		duration:   dur,
		failed:     failed,
	}
}

func (rc *timingReadCloser) Close() error {
	err := rc.ReadCloser.Close()
	rc.duration.WithLabelValues(rc.op).Observe(time.Since(rc.start).Seconds())
	if rc.ok && err != nil {
		rc.failed.WithLabelValues(rc.op).Inc()
		rc.ok = false
	}
	return err
}

func (rc *timingReadCloser) Read(b []byte) (n int, err error) {
	n, err = rc.ReadCloser.Read(b)
	if rc.ok && err != nil && err != io.EOF {
		rc.failed.WithLabelValues(rc.op).Inc()
		rc.ok = false
	}
	return n, err
}

func CreateTemporaryTestBucketName(t testing.TB) string {
	src := rand.NewSource(time.Now().UnixNano())

	// Bucket name need to conform: https://docs.aws.amazon.com/awscloudtrail/latest/userguide/cloudtrail-s3-bucket-naming-requirements.html.
	name := strings.Replace(strings.Replace(fmt.Sprintf("test_%x_%s", src.Int63(), strings.ToLower(t.Name())), "_", "-", -1), "/", "-", -1)
	if len(name) >= 63 {
		name = name[:63]
	}
	return name
}
