package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	minio "github.com/memoio/minio/cmd"
	"github.com/minio/cli"
	"github.com/minio/madmin-go"
	"github.com/minio/pkg/bucket/policy"
	"github.com/minio/pkg/bucket/policy/condition"
	"github.com/mitchellh/go-homedir"
	cli2 "github.com/urfave/cli/v2"
	"golang.org/x/xerrors"

	mclient "github.com/memoio/go-mefs-v2/api/client"
	"github.com/memoio/go-mefs-v2/lib/utils"
	metag "github.com/memoio/go-mefs-v2/lib/utils/etag"
)

var GatewayCmd = &cli2.Command{
	Name:  "gateway",
	Usage: "memo gateway",
	Subcommands: []*cli2.Command{
		gatewayRunCmd,
		gatewayStopCmd,
	},
}

var gatewayRunCmd = &cli2.Command{
	Name:  "run",
	Usage: "run a memo gateway",
	Flags: []cli2.Flag{
		&cli2.StringFlag{
			Name:    "username",
			Aliases: []string{"n"},
			Usage:   "input your user name",
			Value:   "memo",
		},
		&cli2.StringFlag{
			Name:    "password",
			Aliases: []string{"p"},
			Usage:   "input your password",
			Value:   "memoriae",
		},
		&cli2.StringFlag{
			Name:    "endpoint",
			Aliases: []string{"e"},
			Usage:   "input your endpoint",
			Value:   "0.0.0.0:5080",
		},
		&cli2.StringFlag{
			Name:    "console",
			Aliases: []string{"c"},
			Usage:   "input your console for browser",
			Value:   "8080",
		},
	},
	Action: func(cctx *cli2.Context) error {
		var terminate = make(chan os.Signal, 1)
		signal.Notify(terminate, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(terminate)

		username := cctx.String("username")
		if username == "" {
			return xerrors.New("username is nil")
		}

		pwd := cctx.String("password")
		if pwd == "" {
			return xerrors.New("username is nil")
		}
		endPoint := cctx.String("endpoint")
		consoleAddress := cctx.String("console")
		if !strings.Contains(consoleAddress, ":") {
			consoleAddress = ":" + consoleAddress
		}

		// save process id
		pidpath, err := BestKnownPath()
		if err != nil {
			return err
		}
		pid := os.Getpid()
		pids := []byte(strconv.Itoa(pid))
		err = os.WriteFile(path.Join(pidpath, "pid"), pids, 0644)
		if err != nil {
			return err
		}

		err = Start(username, pwd, endPoint, consoleAddress)
		if err != nil {
			return err
		}

		<-terminate
		log.Println("received shutdown signal")
		log.Println("shutdown...")

		return nil
	},
}

var gatewayStopCmd = &cli2.Command{
	Name:  "stop",
	Usage: "stop a memo gateway",
	Action: func(ctx *cli2.Context) error {
		pidpath, err := BestKnownPath()
		if err != nil {
			return err
		}

		pd, _ := ioutil.ReadFile(path.Join(pidpath, "pid"))
		err = kill(string(pd))
		if err != nil {
			return err
		}

		log.Println("gateway gracefully exit...")

		return nil
	},
}

var DefaultPathRoot string = "~/.mefs_gw"

func BestKnownPath() (string, error) {
	mefsPath := DefaultPathRoot
	mefsPath, err := homedir.Expand(mefsPath)
	if err != nil {
		return "", err
	}

	_, err = os.Stat(mefsPath)
	if os.IsNotExist(err) {
		err = os.Mkdir(mefsPath, 0755)
		if err != nil {
			return "", err
		}
	}
	return mefsPath, nil
}

func kill(pid string) error {
	if runtime.GOOS == "windows" {
		err := exec.Command("taskkill", "/T", "/F", "/PID", pid).Start()
		return err
	}
	if runtime.GOOS == "linux" {
		pidi, err := strconv.Atoi(pid)
		if err != nil {
			return err
		}

		p, err := os.FindProcess(pidi)
		if err != nil {
			return err
		}
		return p.Signal(syscall.SIGTERM)
	}
	return errors.New("unsupported os")
}

// Start gateway
func Start(addr, pwd, endPoint, consoleAddress string) error {
	minio.RegisterGatewayCommand(cli.Command{
		Name:            "lfs",
		Usage:           "Mefs Log File System Service (LFS)",
		Action:          mefsGatewayMain,
		HideHelpCommand: true,
	})
	err := os.Setenv("MINIO_ROOT_USER", addr)
	if err != nil {
		return err
	}
	err = os.Setenv("MINIO_ROOT_PASSWORD", pwd)
	if err != nil {
		return err
	}

	rootpath, err := BestKnownPath()
	if err != nil {
		return err
	}

	gwConf := rootpath + "/gwConf"

	// ”memoriae“ is app name
	// "gateway" represents gatewat mode; respective, "server" represents server mode
	// "lfs" is subcommand, should equal to RegisterGatewayCommand{Name}
	go minio.Main([]string{"memoriae", "gateway", "lfs",
		"--address", endPoint, "--config-dir", gwConf, "--console-address", consoleAddress})

	return nil
}

// Handler for 'minio gateway oss' command line.
func mefsGatewayMain(ctx *cli.Context) {
	minio.StartGateway(ctx, &Mefs{"lfs"})
}

// Mefs implements Lfs Gateway.
type Mefs struct {
	host string
}

// Name implements Gateway interface.
func (g *Mefs) Name() string {
	return "mefs"
}

// NewGatewayLayer implements Gateway interface and returns LFS ObjectLayer.
func (g *Mefs) NewGatewayLayer(creds madmin.Credentials) (minio.ObjectLayer, error) {
	gw := &lfsGateway{
		polices: make(map[string]*policy.Policy),
	}

	return gw, nil
}

// Production - oss is production ready.
func (g *Mefs) Production() bool {
	return false
}

// lfsGateway implements gateway.
type lfsGateway struct {
	minio.GatewayUnsupported
	memofs  *MemoFs
	polices map[string]*policy.Policy
}

// Shutdown saves any gateway metadata to disk
// if necessary and reload upon next restart.
func (l *lfsGateway) Shutdown(ctx context.Context) error {
	return nil
}

func (l *lfsGateway) IsEncryptionSupported() bool {
	return true
}

// SetBucketPolicy will set policy on bucket.
func (l *lfsGateway) SetBucketPolicy(ctx context.Context, bucket string, bucketPolicy *policy.Policy) error {
	_, err := l.GetBucketInfo(ctx, bucket)
	if err != nil {
		return err
	}
	l.polices[bucket] = bucketPolicy
	return nil
}

// GetBucketPolicy will get policy on bucket.
func (l *lfsGateway) GetBucketPolicy(ctx context.Context, bucket string) (*policy.Policy, error) {
	if bucket == "favicon.ico" {
		return &policy.Policy{}, nil
	}
	err := l.getMemofs()
	if err != nil {
		return nil, err
	}

	bi, err := l.memofs.GetBucketInfo(ctx, bucket)
	if err != nil {
		return nil, err
	}

	pb, ok := l.polices[bucket]
	if ok {
		return pb, nil
	}

	pp := &policy.Policy{
		ID:      policy.ID(fmt.Sprintf("data: %d, parity: %d", bi.DataCount, bi.ParityCount)),
		Version: policy.DefaultVersion,
		Statements: []policy.Statement{
			policy.NewStatement(
				"",
				policy.Allow,

				policy.NewPrincipal("*"),
				policy.NewActionSet(
					policy.GetObjectAction,
					//policy.ListBucketAction,
				),
				policy.NewResourceSet(
					policy.NewResource(bucket, ""),
					policy.NewResource(bucket, "*"),
				),
				condition.NewFunctions(),
			),
		},
	}

	return pp, nil
}

// StorageInfo is not relevant to LFS backend.
func (l *lfsGateway) StorageInfo(ctx context.Context) (si minio.StorageInfo, errs []error) {
	//log.Println("get StorageInfo")
	si.Backend.Type = madmin.Gateway

	_, closer, err := mclient.NewUserNode(ctx, l.memofs.addr, l.memofs.headers)
	if err == nil {
		closer()
		si.Backend.GatewayOnline = true
	}

	return si, nil
}

// MakeBucketWithLocation creates a new container on LFS backend.
func (l *lfsGateway) MakeBucketWithLocation(ctx context.Context, bucket string, options minio.BucketOptions) error {
	err := l.getMemofs()
	if err != nil {
		return err
	}
	err = l.memofs.MakeBucketWithLocation(ctx, bucket)
	if err != nil {
		return err
	}
	return nil
}

// GetBucketInfo gets bucket metadata.
func (l *lfsGateway) GetBucketInfo(ctx context.Context, bucket string) (bi minio.BucketInfo, err error) {
	//log.Println("get buckte info: ", bucket)
	err = l.getMemofs()
	if err != nil {
		return bi, err
	}
	bucketInfo, err := l.memofs.GetBucketInfo(ctx, bucket)
	if err != nil {
		return bi, err
	}
	bi.Name = bucket
	bi.Created = time.Unix(bucketInfo.GetCTime(), 0).UTC()
	return bi, nil
}

// ListBuckets lists all LFS buckets.
func (l *lfsGateway) ListBuckets(ctx context.Context) (bs []minio.BucketInfo, err error) {
	//log.Println("list bucktes")
	err = l.getMemofs()
	if err != nil {
		return bs, err
	}
	buckets, err := l.memofs.ListBuckets(ctx)
	if err != nil {
		return nil, err
	}

	bs = make([]minio.BucketInfo, 0, len(buckets))
	for _, v := range buckets {
		bs = append(bs, minio.BucketInfo{
			Name:    v.Name,
			Created: time.Unix(v.GetCTime(), 0).UTC(),
		})
	}

	return bs, nil

}

// DeleteBucket deletes a bucket on LFS.
func (l *lfsGateway) DeleteBucket(ctx context.Context, bucket string, opts minio.DeleteBucketOptions) error {
	return minio.NotImplemented{}
}

// ListObjects lists all blobs in LFS bucket filtered by prefix.
func (l *lfsGateway) ListObjects(ctx context.Context, bucket, prefix, marker, delimiter string, maxKeys int) (loi minio.ListObjectsInfo, err error) {
	//log.Println("list object: ", bucket, prefix, marker, delimiter, maxKeys)
	err = l.getMemofs()
	if err != nil {
		return loi, err
	}
	mloi, err := l.memofs.ListObjects(ctx, bucket, prefix, marker, delimiter, maxKeys)
	if err != nil {
		return loi, err
	}

	for _, oi := range mloi.Objects {
		ud := make(map[string]string)
		if oi.UserDefined != nil {
			ud = oi.UserDefined
		}
		//  for s3fs
		ud["x-amz-meta-mode"] = "33204"
		ud["x-amz-meta-mtime"] = time.Unix(oi.GetTime(), 0).Format(utils.SHOWTIME)
		ud["x-amz-meta-state"] = oi.State
		etag, _ := metag.ToString(oi.ETag)
		loi.Objects = append(loi.Objects, minio.ObjectInfo{
			Bucket:      bucket,
			Name:        oi.GetName(),
			ModTime:     time.Unix(oi.GetTime(), 0).UTC(),
			Size:        int64(oi.Size),
			IsDir:       false,
			ETag:        etag,
			UserDefined: ud,
		})
	}

	loi.IsTruncated = mloi.IsTruncated
	loi.NextMarker = mloi.NextMarker
	loi.Prefixes = mloi.Prefixes

	return loi, nil
}

// ListObjectsV2 lists all blobs in LFS bucket filtered by prefix
func (l *lfsGateway) ListObjectsV2(ctx context.Context, bucket, prefix, continuationToken, delimiter string, maxKeys int,
	fetchOwner bool, startAfter string) (loiv2 minio.ListObjectsV2Info, err error) {
	//log.Println("list objects v2: ", bucket, prefix, continuationToken, delimiter, maxKeys, startAfter)
	marker := continuationToken
	if marker == "" {
		marker = startAfter
	}

	loi, err := l.ListObjects(ctx, bucket, prefix, marker, delimiter, maxKeys)
	if err != nil {
		return loiv2, err
	}

	loiv2 = minio.ListObjectsV2Info{
		IsTruncated:           loi.IsTruncated,
		ContinuationToken:     continuationToken,
		NextContinuationToken: loi.NextMarker,
		Objects:               loi.Objects,
		Prefixes:              loi.Prefixes,
	}

	return loiv2, err
}

// GetObjectNInfo - returns object info and locked object ReadCloser
func (l *lfsGateway) GetObjectNInfo(ctx context.Context, bucket, object string, rs *minio.HTTPRangeSpec, h http.Header, lockType minio.LockType, opts minio.ObjectOptions) (gr *minio.GetObjectReader, err error) {
	//log.Println("get objectn: ", bucket, object)
	objInfo, err := l.GetObjectInfo(ctx, bucket, object, opts)
	if err != nil {
		return nil, minio.ErrorRespToObjectError(err, bucket, object)
	}

	fn, off, length, err := minio.NewGetObjectReader(rs, objInfo, opts)
	if err != nil {
		return nil, minio.ErrorRespToObjectError(err, bucket, object)
	}

	pr, pw := io.Pipe()
	go func() {
		err := l.GetObject(ctx, bucket, object, off, length, pw, objInfo.ETag, opts)
		pw.CloseWithError(err)
	}()

	// Setup cleanup function to cause the above go-routine to
	// exit in case of partial read
	pipeCloser := func() { pr.Close() }
	return fn(pr, h, pipeCloser)
}

// GetObject reads an object on LFS. Supports additional
// parameters like offset and length which are synonymous with
// HTTP Range requests.
//
// startOffset indicates the starting read location of the object.
// length indicates the total length of the object.
func (l *lfsGateway) GetObject(ctx context.Context, bucketName, objectName string, startOffset, length int64, writer io.Writer, etag string, o minio.ObjectOptions) error {
	err := l.getMemofs()
	if err != nil {
		return err
	}
	err = l.memofs.GetObject(ctx, bucketName, objectName, startOffset, length, writer)
	if err != nil {
		return err
	}
	return nil
}

// GetObjectInfo reads object info and replies back ObjectInfo.
func (l *lfsGateway) GetObjectInfo(ctx context.Context, bucket, object string, opts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
	err = l.getMemofs()
	if err != nil {
		return objInfo, err
	}
	moi, err := l.memofs.GetObjectInfo(ctx, bucket, object)
	if err != nil {
		return objInfo, err
	}

	ud := make(map[string]string)
	if moi.UserDefined != nil {
		ud = moi.UserDefined
	}
	// for s3fs
	ud["x-amz-meta-mode"] = "33204"
	ud["x-amz-meta-mtime"] = time.Unix(moi.GetTime(), 0).Format(utils.SHOWTIME)
	ud["x-amz-meta-state"] = moi.State
	// need handle ETag
	etag, _ := metag.ToString(moi.ETag)
	oi := minio.ObjectInfo{
		Bucket:      bucket,
		Name:        moi.Name,
		ModTime:     time.Unix(moi.GetTime(), 0),
		Size:        int64(moi.Size),
		ETag:        etag,
		IsDir:       false,
		UserDefined: ud,
	}

	return oi, nil
}

// PutObject creates a new object with the incoming data.
func (l *lfsGateway) PutObject(ctx context.Context, bucket, object string, r *minio.PutObjReader, opts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
	err = l.getMemofs()
	if err != nil {
		return objInfo, err
	}
	_, err = l.memofs.GetObjectInfo(ctx, bucket, object)
	if err == nil {
		mtime := time.Now().Format("20060102T150405")
		suffix := path.Ext(object)
		if len(suffix) > 0 && len(object) > len(suffix) {
			object = object[:len(object)-len(suffix)] + "-" + mtime + suffix
		} else {
			object = object + "-" + mtime
		}
	}
	moi, err := l.memofs.PutObject(ctx, bucket, object, r, opts.UserDefined)
	if err != nil {
		return objInfo, err
	}
	etag, _ := metag.ToString(moi.ETag)
	oi := minio.ObjectInfo{
		Bucket:  bucket,
		Name:    moi.Name,
		ModTime: time.Unix(moi.GetTime(), 0),
		Size:    int64(moi.Size),
		ETag:    etag,
		IsDir:   false,
	}

	if moi.UserDefined != nil {
		oi.UserDefined = moi.UserDefined
		oi.UserDefined["x-amz-meta-state"] = moi.State
	}

	return oi, nil
}

// CopyObject copies an object from source bucket to a destination bucket.
func (l *lfsGateway) CopyObject(ctx context.Context, srcBucket, srcObject, dstBucket, dstObject string, srcInfo minio.ObjectInfo, srcOpts, dstOpts minio.ObjectOptions) (objInfo minio.ObjectInfo, err error) {
	return objInfo, minio.NotImplemented{}
}

// DeleteObject deletes a blob in bucket.
func (l *lfsGateway) DeleteObject(ctx context.Context, bucket, object string, opts minio.ObjectOptions) (minio.ObjectInfo, error) {
	return minio.ObjectInfo{}, minio.NotImplemented{}
}

func (l *lfsGateway) DeleteObjects(ctx context.Context, bucket string, objects []minio.ObjectToDelete, opts minio.ObjectOptions) ([]minio.DeletedObject, []error) {
	errs := make([]error, len(objects))
	dobjects := make([]minio.DeletedObject, len(objects))
	for idx := range objects {
		errs[idx] = minio.NotImplemented{}

	}

	return dobjects, errs
}

// IsCompressionSupported returns whether compression is applicable for this layer.
func (l *lfsGateway) IsCompressionSupported() bool {
	return false
}

func (l *lfsGateway) StatObject(ctx context.Context, bucket, object string, opts minio.ObjectOptions) (minio.ObjectInfo, error) {
	//log.Println("state object info: ", bucket, object)
	return minio.ObjectInfo{}, minio.NotImplemented{}
}
