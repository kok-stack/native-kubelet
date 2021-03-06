package native

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/containerd/containerd/images"
	cc "github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/directory"
	"github.com/containers/image/v5/docker/archive"
	"github.com/containers/image/v5/signature"
	"github.com/containers/image/v5/transports"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	"github.com/flytam/filenamify"
	"github.com/kok-stack/native-kubelet/trace"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"git.mills.io/prologic/bitcask"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const pullLogPrefix = "native-kubelet-pullImage-"
const manifestFileName = "manifest.json"

var insecurePolicy = []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`)

type ImagePulling struct {
	imageName string
	ch        chan interface{}
	f         *os.File
}

func NewImagePulling(imageName string) *ImagePulling {
	return &ImagePulling{
		imageName: imageName,
		ch:        make(chan interface{}),
	}
}

type ImageManager struct {
	imagePath string
	pulling   sync.Map
	imageDb   *bitcask.Bitcask
	max       int
}

func NewImageManager(imagePath string, db *bitcask.Bitcask, max int) *ImageManager {
	return &ImageManager{
		imagePath: imagePath,
		imageDb:   db,
		max:       max,
	}
}

type PullImageOpts struct {
	SrcImage string // docker://ccr.ccs.tencentyun.com/k8s-test/test:oci-test-v1

	DockerAuthConfig            *types.DockerAuthConfig
	DockerBearerRegistryToken   string
	DockerRegistryUserAgent     string
	DockerInsecureSkipTLSVerify types.OptionalBool

	Timeout    time.Duration
	RetryCount int
	//Stdout     io.Writer
}

func (m *ImageManager) PullImage(ctx context.Context, opts PullImageOpts) error {
	ctx, span := trace.StartSpan(ctx, "ImageManager.PullImage")
	defer span.End()
	name := opts.SrcImage
	srcRef, err := alltransports.ParseImageName(name)
	if err != nil {
		span.SetStatus(err)
		return err
	}
	dest, imageDir, err := imageDestDir(m.imagePath, opts.SrcImage)
	if err != nil {
		span.SetStatus(err)
		return err
	}
	//???????????????????????????,??????????????????
	if err := createDestDir(filepath.Dir(imageDir)); err != nil {
		return err
	}
	destRef, err := alltransports.ParseImageName(dest)
	if err != nil {
		span.SetStatus(err)
		return err
	}

	sourceCtx := &types.SystemContext{
		DockerAuthConfig:            opts.DockerAuthConfig,
		DockerBearerRegistryToken:   opts.DockerBearerRegistryToken,
		DockerRegistryUserAgent:     opts.DockerRegistryUserAgent,
		DockerInsecureSkipTLSVerify: opts.DockerInsecureSkipTLSVerify,
	}
	destinationCtx := &types.SystemContext{}

	policy, err := signature.NewPolicyFromBytes(insecurePolicy)
	if err != nil {
		span.SetStatus(err)
		return err
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		span.SetStatus(err)
		return err
	}
	if opts.Timeout == 0 {
		opts.Timeout = time.Duration(m.max) * time.Second
	}
	subCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

check:
	//????????????????????????
	ok := m.imageDb.Has([]byte(name))
	if ok {
		span.Logger().Debug("?????????????????????,????????????")
		return nil
	}
	//???????????????,??????????????????pull
	v, ok := m.pulling.Load(name)
	if ok {
		//??????pull,?????????pull??????
		span.Logger().Debug("????????????pull,??????pull??????")
		pull := v.(*ImagePulling)
		<-pull.ch
		span.Logger().Debug("??????pull??????")
		goto check
	}
	//?????????pull,?????????pull
	span.Logger().Debug("?????????pull,????????????pull")

	pulling := NewImagePulling(name)
	m.pulling.LoadOrStore(name, pulling)
	defer close(pulling.ch)
	defer m.pulling.Delete(name)
	logName := strconv.Itoa(rand.Intn(time.Now().Nanosecond()))

	err = deleteExistImage(imageDir)

	pulling.f, err = os.OpenFile(filepath.Join(os.TempDir(), fmt.Sprintf("%s-%s", pullLogPrefix, logName)), os.O_CREATE|os.O_RDWR, 0755)
	if err != nil {
		span.SetStatus(err)
		return err
	}
	defer pulling.f.Close()

	_, err = cc.Image(subCtx, policyContext, destRef, srcRef, &cc.Options{
		ReportWriter:       pulling.f,
		SourceCtx:          sourceCtx,
		DestinationCtx:     destinationCtx,
		ImageListSelection: cc.CopySystemImage,
	})
	if err != nil {
		span.Logger().Error("pull????????????")
		span.SetStatus(err)
		return err
	}
	return m.imageDb.Put([]byte(name), []byte(dest))
}

func deleteExistImage(dir string) error {
	if err := os.Remove(dir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return nil
}

func createDestDir(dir string) error {
	_, err := os.Stat(dir)
	if err == nil {
		return nil
	}
	if os.IsNotExist(err) {
		if err := os.MkdirAll(dir, os.ModePerm); err != nil {
			return err
		}
		return nil
	}
	return err
}

func (m *ImageManager) UnzipImage(ctx context.Context, image string, workdir string) error {
	ctx, span := trace.StartSpan(ctx, "ImageManager.UnzipImage")
	defer span.End()
	imageDir := getImageWorkDir(workdir)
	ctx = span.WithFields(ctx, map[string]interface{}{
		"image":    image,
		"workdir":  workdir,
		"imageDir": imageDir,
	})
	policy, err := signature.NewPolicyFromBytes(insecurePolicy)
	if err != nil {
		span.SetStatus(err)
		return err
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		span.SetStatus(err)
		return err
	}
	//??????workdir
	if err := createDestDir(imageDir); err != nil {
		span.SetStatus(err)
		return err
	}
	destRef, err := directory.Transport.ParseReference(imageDir)
	if err != nil {
		span.SetStatus(err)
		return err
	}
	//??????image???path
	imagePath, err := m.imageDb.Get([]byte(dockerImageName(image)))
	if err != nil {
		span.SetStatus(err)
		return err
	}
	//??????
	srcRef, err := alltransports.ParseImageName(string(imagePath))
	if err != nil {
		span.SetStatus(err)
		return err
	}
	sourceCtx := &types.SystemContext{}
	destinationCtx := &types.SystemContext{}

	//????????????
	if _, err := cc.Image(ctx, policyContext, destRef, srcRef, &cc.Options{
		ReportWriter:       nil,
		SourceCtx:          sourceCtx,
		DestinationCtx:     destinationCtx,
		ImageListSelection: cc.CopySystemImage,
	}); err != nil {
		span.SetStatus(err)
		return err
	}
	span.Logger().Debug("??????tar?????????,??????????????????layer")
	//?????? "???"
	content, err := ioutil.ReadFile(manifestDir(imageDir))
	if err != nil {
		span.Logger().Error("????????????layer??????")
		span.SetStatus(err)
		return err
	}
	manifest := &v1.Manifest{}
	err = json.Unmarshal(content, manifest)
	if err != nil {
		span.Logger().Error("????????????Manifest??????")
		span.SetStatus(err)
		return err
	}
	for _, layer := range manifest.Layers {
		switch layer.MediaType {
		case images.MediaTypeDockerSchema2LayerGzip:
			err = UnTar(getLayerFilePath(imageDir, layer.Digest), containerWorkDir(workdir))
			if err != nil {
				span.Logger().Error("???tar??????????????????")
				span.SetStatus(err)
				return err
			}
		default:
			err := fmt.Errorf("unsupport image %s layer %s media type:%s", image, layer.Digest.Encoded(), layer.MediaType)
			span.SetStatus(err)
			return err
		}
	}

	return nil
}

func getLayerFilePath(imageDir string, digest digest.Digest) string {
	return filepath.Join(imageDir, digest.Encoded())
}

func containerWorkDir(workdir string) string {
	return filepath.Join(workdir, "container")
}

func manifestDir(workdir string) string {
	return filepath.Join(workdir, manifestFileName)
}

func getImageWorkDir(workdir string) string {
	return filepath.Join(workdir, "image")
}

/*
path=/path
imageName=docker://imagename

docker-archive:/path/imagename.tar.gz
/path/imagename.tar.gz
*/
func imageDestDir(path string, imageName string) (string, string, error) {
	names := append(transports.ListNames(), "//")
	replaceNames := make([]string, len(names)*2)
	for i, n := range names {
		replaceNames[i*2] = n
		replaceNames[i*2+1] = ""
	}
	replacer := strings.NewReplacer(replaceNames...)
	replace := replacer.Replace(imageName)
	imageName = replace
	s, err := filenamify.Filenamify(imageName, filenamify.Options{Replacement: "-"})
	if err != nil {
		return "", "", err
	}
	filep := fmt.Sprintf("%s.tar.gz", filepath.Join(path, s))
	return fmt.Sprintf("%s:%s", archive.Transport.Name(), filep), filep, nil
}
