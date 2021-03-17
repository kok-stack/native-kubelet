package native

import (
	"context"
	"fmt"
	"github.com/containers/common/pkg/retry"
	cc "github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/docker/archive"
	"github.com/containers/image/v5/signature"
	"github.com/containers/image/v5/transports"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	"github.com/flytam/filenamify"
	"github.com/prologic/bitcask"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const pullLogPrefix = "native-kubelet-pullImage-"

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
}

func NewImageManager(imagePath string, db *bitcask.Bitcask) *ImageManager {
	return &ImageManager{
		imagePath: imagePath,
		imageDb:   db,
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
	name := opts.SrcImage
	srcRef, err := alltransports.ParseImageName(name)
	if err != nil {
		return fmt.Errorf("Invalid source name %s: %v", name, err)
	}
	dest, filep, err := imageDestDir(m.imagePath, opts.SrcImage)
	if err != nil {
		return err
	}
	destRef, err := alltransports.ParseImageName(dest)
	if err != nil {
		return fmt.Errorf("Invalid destination name %s: %v", dest, err)
	}

	sourceCtx := &types.SystemContext{
		DockerAuthConfig:            opts.DockerAuthConfig,
		DockerBearerRegistryToken:   opts.DockerBearerRegistryToken,
		DockerRegistryUserAgent:     opts.DockerRegistryUserAgent,
		DockerInsecureSkipTLSVerify: opts.DockerInsecureSkipTLSVerify,
	}
	destinationCtx := &types.SystemContext{}

	policy, err := signature.DefaultPolicy(nil)
	if err != nil {
		return err
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		return err
	}
	subCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

check:
	//检查是否存在镜像
	ok := m.imageDb.Has([]byte(name))
	fmt.Println("检查镜像是否存在:", ok)
	if ok {
		return nil
	}
	//如果不存在,检查是否正在pull
	v, ok := m.pulling.Load(name)
	fmt.Println("检查是否正在pull:", ok)
	if ok {
		//正在pull,则等到pull结束
		fmt.Println("正在pull,则等到pull结束")
		pull := v.(*ImagePulling)
		<-pull.ch
		fmt.Println("pull结束")
		goto check
	}
	//没有在pull,则执行pull
	fmt.Println("没有在pull,则执行pull")

	pulling := NewImagePulling(name)
	m.pulling.LoadOrStore(name, pulling)
	defer close(pulling.ch)
	defer m.pulling.Delete(name)
	logName := strconv.Itoa(rand.Intn(time.Now().Nanosecond()))

	pulling.f, err = os.OpenFile(filepath.Join(os.TempDir(), fmt.Sprintf("%s-%s", pullLogPrefix, logName)), os.O_CREATE, 0755)
	if err != nil {
		return err
	}
	defer pulling.f.Close()

	err = retry.RetryIfNecessary(subCtx, func() error {
		_, err = cc.Image(subCtx, policyContext, destRef, srcRef, &cc.Options{
			ReportWriter:       pulling.f,
			SourceCtx:          sourceCtx,
			DestinationCtx:     destinationCtx,
			ImageListSelection: cc.CopySystemImage,
		})
		return err
	}, &retry.RetryOptions{
		MaxRetry: opts.RetryCount,
		Delay:    time.Microsecond * 100,
	})
	if err != nil {
		return err
	}
	return m.imageDb.Put([]byte(name), []byte(filep))
}

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
