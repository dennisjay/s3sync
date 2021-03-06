package main

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/eapache/channels"
	"github.com/fsnotify/fsnotify"
	"github.com/gobwas/glob"
	"github.com/karrick/godirwalk"
	"gopkg.in/yaml.v2"
	"io"
	"io/ioutil"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//Object contain content and metadata of S3 object
type Object struct {
	Key         string
	ETag        string
	Mtime       time.Time
	Content     []byte
	ContentType string
}

//SyncGroup contain Source and Target configuration. Thread safe
type SyncGroup struct {
	Source Storage
	Target Storage
}

//Storage interface
type Storage interface {
	List(ch chan<- Object) error
	Watch(ch chan<- Object) error
	PutObject(object *Object) error
	GetObjectContent(obj *Object) error
	GetObjectMeta(obj *Object) error
}

type HeaderConfigPatterns struct {
	Pattern         string
	Config          s3.PutObjectInput
	PatternCompiled glob.Glob
}

type HeaderConfig struct {
	Patterns []HeaderConfigPatterns
}

//AWSStorage configuration
type AWSStorage struct {
	awsSvc        *s3.S3
	awsSession    *session.Session
	awsBucket     string
	prefix        string
	acl           string
	keysPerReq    int64
	workers       uint
	retry         uint
	retryInterval time.Duration
	headerConfig  HeaderConfig
}

//FSStorage configuration
type FSStorage struct {
	dir      string
	filePerm os.FileMode
	dirPerm  os.FileMode
	workers  uint
}

func readYamlConfig(headersConfig string) HeaderConfig {

	var result HeaderConfig

	if len(headersConfig) <= 0 {
		return result
	}

	content, err := ioutil.ReadFile(headersConfig)
	if err != nil {
		log.Fatalf("error: %v", err)
		return result
	}

	err = yaml.UnmarshalStrict([]byte(content), &result)
	if err != nil {
		log.Fatalf("error: %v", err)
		return result
	}

	for _, pat := range result.Patterns {
		pat.PatternCompiled = glob.MustCompile(pat.Pattern)
	}

	return result
}

//NewAWSStorage return new configured S3 storage
func NewAWSStorage(awsAccessKey, awsSecretKey, awsRegion, endpoint, bucketName, prefix, acl string, keysPerReq int64, workers, retry uint, retryInterval time.Duration, headersConfig string) (storage AWSStorage) {
	awsConfig := aws.NewConfig()
	awsConfig.S3ForcePathStyle = aws.Bool(true)
	awsConfig.CredentialsChainVerboseErrors = aws.Bool(true)

	if awsAccessKey != "" && awsSecretKey != "" {
		cred := credentials.NewStaticCredentials(awsAccessKey, awsSecretKey, "")
		awsConfig.WithCredentials(cred)
	} else {
		cred := credentials.NewChainCredentials(
			[]credentials.Provider{
				&credentials.EnvProvider{},
				&credentials.SharedCredentialsProvider{},
			})
		awsConfig.WithCredentials(cred)
	}

	awsConfig.Region = aws.String(awsRegion)
	if endpoint != "" {
		awsConfig.Endpoint = aws.String(endpoint)
	}
	storage.awsBucket = bucketName
	storage.awsSession = session.Must(session.NewSession(awsConfig))
	storage.awsSvc = s3.New(storage.awsSession)
	storage.prefix = prefix
	storage.acl = acl
	storage.keysPerReq = keysPerReq
	storage.workers = workers
	storage.retry = retry
	storage.retryInterval = retryInterval

	storage.headerConfig = readYamlConfig(headersConfig)

	return storage
}

//NewFSStorage return new configured FS storage
func NewFSStorage(dir string, filePerm, dirPerm os.FileMode, workers uint) (storage FSStorage) {
	storage.dir = filepath.Clean(dir) + "/"
	storage.filePerm = filePerm
	storage.dirPerm = dirPerm
	storage.workers = workers
	return storage
}

//List S3 bucket and send founded objects to chan
func (storage AWSStorage) List(output chan<- Object) error {
	prefixChan := channels.NewInfiniteChannel()
	listResultChan := make(chan error, storage.workers)
	wg := sync.WaitGroup{}
	stopListing := false

	listObjectsRecursive := func(prefixChan *channels.InfiniteChannel, output chan<- Object) {
		listObjectsFn := func(p *s3.ListObjectsOutput, lastPage bool) bool {
			for _, o := range p.CommonPrefixes {
				wg.Add(1)
				prefixChan.In() <- aws.StringValue(o.Prefix)
			}
			for _, o := range p.Contents {
				atomic.AddUint64(&counter.totalObjCnt, 1)
				output <- Object{Key: aws.StringValue(o.Key), ETag: aws.StringValue(o.ETag), Mtime: aws.TimeValue(o.LastModified)}
			}
			return true // continue paging
		}

		for prefix := range prefixChan.Out() {
			for i := uint(0); i <= storage.retry; i++ {
				if stopListing {
					wg.Done()
					return
				}
				err := storage.awsSvc.ListObjectsPages(&s3.ListObjectsInput{
					Bucket:    aws.String(storage.awsBucket),
					Prefix:    aws.String(prefix.(string)),
					MaxKeys:   aws.Int64(storage.keysPerReq),
					Delimiter: aws.String("/"),
				}, listObjectsFn)

				if (err != nil) && (i == storage.retry) {
					wg.Done()
					listResultChan <- err
					break
				} else if err == nil {
					wg.Done()
					break
				} else {
					log.Debugf("S3 listing failed with error: %s", err)
					time.Sleep(storage.retryInterval)
					continue
				}
			}
		}
	}

	for i := storage.workers; i != 0; i-- {
		go listObjectsRecursive(prefixChan, output)
	}

	// Start listing from storage.prefix
	wg.Add(1)
	prefixChan.In() <- storage.prefix

	go func() {
		wg.Wait()
		prefixChan.Close()
		listResultChan <- nil
	}()

	select {
	case msg := <-listResultChan:
		stopListing = true
		wg.Wait()
		close(output)
		return msg
	}
}

//Watch S3 bucket and send found objects to chan
func (storage AWSStorage) Watch(output chan<- Object) error {
	return nil
}

//PutObject to bucket
func (storage AWSStorage) PutObject(obj *Object) error {
	key := obj.Key
	input := s3.PutObjectInput{
		Bucket:      aws.String(storage.awsBucket),
		Key:         aws.String(filepath.Join(storage.prefix, key)),
		Body:        bytes.NewReader(obj.Content),
		ContentType: aws.String(obj.ContentType),
		ACL:         aws.String(storage.acl),
	}

	for _, pat := range storage.headerConfig.Patterns {
		pat.PatternCompiled = glob.MustCompile(pat.Pattern)
		if pat.PatternCompiled.Match(key) {
			if pat.Config.CacheControl != nil {
				input.SetCacheControl(*pat.Config.CacheControl)
			}
			if pat.Config.ContentType != nil {
				input.SetContentType(*pat.Config.ContentType)
			}
			if pat.Config.Tagging != nil {
				input.SetTagging(*pat.Config.Tagging)
			}
		}
	}

	_, err := storage.awsSvc.PutObject(&input)
	if err != nil {
		return err
	}
	return nil
}

//GetObjectContent download object content from S3
func (storage AWSStorage) GetObjectContent(obj *Object) error {
	result, err := storage.awsSvc.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(storage.awsBucket),
		Key:    aws.String(obj.Key),
	})
	if err != nil {
		return err
	}

	obj.Content, err = ioutil.ReadAll(result.Body)
	if err != nil {
		return err
	}

	obj.ContentType = aws.StringValue(result.ContentType)
	obj.ETag = aws.StringValue(result.ETag)
	obj.Mtime = aws.TimeValue(result.LastModified)
	return nil
}

//GetObjectMeta update object metadata from S3
func (storage AWSStorage) GetObjectMeta(obj *Object) error {
	result, err := storage.awsSvc.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(storage.awsBucket),
		Key:    aws.String(filepath.Join(storage.prefix, obj.Key)),
	})
	if err != nil {
		return err
	}

	obj.ContentType = aws.StringValue(result.ContentType)
	obj.ETag = aws.StringValue(result.ETag)
	if len(obj.ETag) == 34 { //corect MD5 hash with qoutes
		obj.ETag = obj.ETag[1:33]
	}
	obj.Mtime = aws.TimeValue(result.LastModified)
	return nil
}

//List FS and send founded objects to chan
func (storage FSStorage) List(output chan<- Object) error {
	prefixChan := channels.NewInfiniteChannel()
	listResultChan := make(chan error, storage.workers)
	wg := sync.WaitGroup{}
	stopListing := false

	listObjectsRecursive := func(prefixChan *channels.InfiniteChannel, output chan<- Object) {
		buffer := make([]byte, 1024*64)

		for prefix := range prefixChan.Out() {
			if stopListing {
				wg.Done()
				return
			}
			dirents, err := godirwalk.ReadDirents(prefix.(string), buffer)

			if err != nil {
				wg.Done()
				listResultChan <- err
				return
			}

			for _, dirent := range dirents {
				path := filepath.Join(prefix.(string), dirent.Name())
				if dirent.IsDir() {
					wg.Add(1)
					prefixChan.In() <- path
					continue
				} else {
					atomic.AddUint64(&counter.totalObjCnt, 1)
					output <- Object{Key: strings.TrimPrefix(path, storage.dir)}
				}
			}
			wg.Done()
		}
	}

	for i := storage.workers; i != 0; i-- {
		go listObjectsRecursive(prefixChan, output)
	}

	// Start listing from storage.prefix
	wg.Add(1)
	prefixChan.In() <- storage.dir

	go func() {
		wg.Wait()
		prefixChan.Close()
		listResultChan <- nil
	}()

	select {
	case msg := <-listResultChan:
		stopListing = true
		wg.Wait()
		close(output)
		return msg
	}
}

//Watch FS and send results to chan
func (storage FSStorage) Watch(output chan<- Object) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}

	done := make(chan error)
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				log.Println("event:", event)
				if event.Op&fsnotify.Create == fsnotify.Create {
					log.Println("modified file:", event.Name)

					atomic.AddUint64(&counter.totalObjCnt, 1)
					output <- Object{Key: strings.TrimPrefix(event.Name, storage.dir)}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				if err != nil {
					log.Fatal(err)
				}
				done <- err
				return
			}
		}
	}()

	err = watcher.Add(storage.dir)
	if err != nil {
		log.Fatal(err)
	}

	select {
	case msg := <-done:
		watcher.Close()
		close(output)
		return msg
	}
}

//PutObject save object to FS
func (storage FSStorage) PutObject(obj *Object) error {
	destPath := filepath.Join(storage.dir, obj.Key)
	err := os.MkdirAll(filepath.Dir(destPath), storage.dirPerm)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(destPath, obj.Content, storage.filePerm)
	if err != nil {
		return err
	}
	return nil
}

//GetObjectContent read object content from FS
func (storage FSStorage) GetObjectContent(obj *Object) (err error) {
	destPath := filepath.Join(storage.dir, obj.Key)
	obj.Content, err = ioutil.ReadFile(destPath)
	if err != nil {
		return err
	}

	fh, err := os.Open(destPath)
	if err != nil {
		return err
	}
	defer fh.Close()

	_, err = fh.Read(obj.Content)
	if err != nil && err != io.EOF {
		return err
	}

	obj.ContentType = mime.TypeByExtension(filepath.Ext(destPath))
	fileInfo, err := os.Stat(destPath)
	if err != nil {
		return err
	}
	obj.ETag = etagFromContent(obj.Content)
	obj.Mtime = fileInfo.ModTime()
	return nil
}

//GetObjectMeta update object metadata from FS
func (storage FSStorage) GetObjectMeta(obj *Object) (err error) {
	destPath := filepath.Join(storage.dir, obj.Key)

	obj.ContentType = mime.TypeByExtension(filepath.Ext(destPath))
	fileInfo, err := os.Stat(destPath)
	if err != nil {
		return err
	}
	obj.ETag = ""
	obj.Mtime = fileInfo.ModTime()
	return nil
}

//etagFromContent generate ETAG from contents of file
func etagFromContent(content []byte) string {
	buf := new(bytes.Buffer)
	err := binary.Write(buf, binary.LittleEndian, content)
	if err != nil {
		return ""
	}
	hasher := md5.New()
	hasher.Write(buf.Bytes())
	return hex.EncodeToString(hasher.Sum(nil))
}
