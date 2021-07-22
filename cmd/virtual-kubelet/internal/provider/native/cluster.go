package native

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/kok-stack/native-kubelet/cmd/virtual-kubelet/internal/provider"
	"github.com/kok-stack/native-kubelet/log"
	"github.com/kok-stack/native-kubelet/node/api"
	"github.com/kok-stack/native-kubelet/trace"
	"git.mills.io/prologic/bitcask"
	"io"
	"io/ioutil"
	v1 "k8s.io/api/core/v1"
	errors2 "k8s.io/apimachinery/pkg/api/errors"
	"path/filepath"
	"time"
)

const (
	namespaceKey     = "namespace"
	nameKey          = "name"
	containerNameKey = "containerName"
	nodeNameKey      = "nodeName"

	DbPath        = "data"
	ImagePath     = "images"
	ContainerPath = "containers"
)

type config struct {
	WorkDir    string `json:"work_dir"`
	MaxTimeout int    `json:"max_timeout"`
}

type Provider struct {
	initConfig provider.InitConfig
	startTime  time.Time
	config     *config

	podHandler  *PodEventHandler
	nodeHandler *NodeEventHandler

	imageManager   *ImageManager
	db             *bitcask.Bitcask
	processManager *ProcessManager
}

func (p *Provider) NotifyPods(ctx context.Context, f func(*v1.Pod)) {
	subCtx, span := trace.StartSpan(ctx, "Provider.NotifyPods")
	defer span.End()
	p.podHandler.notifyFunc = f
	p.podHandler.start(subCtx)
}

func (p *Provider) CreatePod(ctx context.Context, pod *v1.Pod) error {
	ctx, span := trace.StartSpan(ctx, "Provider.CreatePod")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, pod.Namespace, nameKey, pod.Name, nodeNameKey, p.initConfig.NodeName)
	log.G(ctx).Info("开始创建pod")

	err := p.processManager.RunPod(ctx, pod)
	if err != nil {
		span.Logger().Error("创建pod错误", err.Error())
		span.SetStatus(err)
		return err
	}
	return nil
}

func (p *Provider) UpdatePod(ctx context.Context, pod *v1.Pod) error {
	//up-->down
	ctx, span := trace.StartSpan(ctx, "Provider.UpdatePod")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, pod.Namespace, nameKey, pod.Name, nodeNameKey, p.initConfig.NodeName)

	err := fmt.Errorf("unsupport UpdatePod %s/%s", pod.Namespace, pod.Name)

	span.Logger().Error("更新pod错误", err.Error())
	span.SetStatus(err)
	return err
}

func (p *Provider) DeletePod(ctx context.Context, pod *v1.Pod) error {
	//up-->down
	ctx, span := trace.StartSpan(ctx, "Provider.DeletePod")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, pod.Namespace, nameKey, pod.Name, nodeNameKey, p.initConfig.NodeName)

	err := p.processManager.DeletePod(ctx, pod)
	if (err != nil && errors2.IsNotFound(err)) || err == nil {
		return nil
	} else {
		span.Logger().Error("删除pod错误", err.Error())
		span.SetStatus(err)
	}
	return err
}

func (p *Provider) GetPod(ctx context.Context, namespace, name string) (*v1.Pod, error) {
	ctx, span := trace.StartSpan(ctx, "Provider.GetPod")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, namespace, nameKey, name, nodeNameKey, p.initConfig.NodeName)

	pod, err := p.processManager.getPod(ctx, namespace, name)
	if err != nil {
		span.Logger().Error("get pod错误", err.Error())
		span.SetStatus(err)
	}
	return pod, err
}

func (p *Provider) GetPodStatus(ctx context.Context, namespace, name string) (*v1.PodStatus, error) {
	ctx, span := trace.StartSpan(ctx, "Provider.GetPodStatus")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, namespace, nameKey, name, nodeNameKey, p.initConfig.NodeName)

	pod, err := p.GetPod(ctx, namespace, name)
	if err != nil {
		span.Logger().Error("获取pod出现错误", err.Error())
		span.SetStatus(err)
		return nil, err
	}
	return &pod.Status, nil
}

func (p *Provider) GetPods(ctx context.Context) ([]*v1.Pod, error) {
	ctx, span := trace.StartSpan(ctx, "Provider.GetPods")
	defer span.End()
	ctx = addAttributes(ctx, span, nodeNameKey, p.initConfig.NodeName)

	pods, err := p.processManager.getPods(ctx)
	if err != nil {
		span.Logger().Error("获取pod列表出现错误", err.Error())
		span.SetStatus(err)
	}
	return pods, err
}

func (p *Provider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	ctx, span := trace.StartSpan(ctx, "Provider.GetContainerLogs")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, namespace, nameKey, podName, nodeNameKey, p.initConfig.NodeName)
	reader, err := p.processManager.getPodLog(ctx, namespace, podName, containerName, opts)
	if err != nil {
		span.Logger().Error("获取pod日志出现错误", err.Error())
		span.SetStatus(err)
	}
	return reader, nil
}

func (p *Provider) RunInContainer(ctx context.Context, namespace, podName, containerName string, cmd []string, attach api.AttachIO) error {
	ctx, span := trace.StartSpan(ctx, "Provider.RunInContainer")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, namespace, nameKey, podName, nodeNameKey, p.initConfig.NodeName)
	//
	//defer func() {
	//	if attach.Stdout() != nil {
	//		attach.Stdout().Close()
	//	}
	//	if attach.Stderr() != nil {
	//		attach.Stderr().Close()
	//	}
	//}()
	//
	//req := p.downClientSet.CoreV1().RESTClient().
	//	Post().
	//	Namespace(namespace).
	//	Resource("pods").
	//	Name(podName).
	//	SubResource("exec").
	//	Timeout(0).
	//	VersionedParams(&v1.PodExecOptions{
	//		Container: containerName,
	//		Command:   cmd,
	//		Stdin:     attach.Stdin() != nil,
	//		Stdout:    attach.Stdout() != nil,
	//		Stderr:    attach.Stderr() != nil,
	//		TTY:       attach.TTY(),
	//	}, scheme.ParameterCodec)
	//
	//exec, err := remotecommand.NewSPDYExecutor(p.downConfig, "POST", req.URL())
	//if err != nil {
	//	err := fmt.Errorf("could not make remote command: %v", err)
	//	span.Logger().Error(err.Error())
	//	span.SetStatus(err)
	//	return err
	//}
	//
	//ts := &termSize{attach: attach}
	//
	//err = exec.Stream(remotecommand.StreamOptions{
	//	Stdin:             attach.Stdin(),
	//	Stdout:            attach.Stdout(),
	//	Stderr:            attach.Stderr(),
	//	Tty:               attach.TTY(),
	//	TerminalSizeQueue: ts,
	//})
	//if err != nil {
	//	span.Logger().Error(err.Error())
	//	span.SetStatus(err)
	//	return err
	//}

	return nil
}

func (p *Provider) ConfigureNode(ctx context.Context, node *v1.Node) {
	p.nodeHandler.configureNode(ctx, node)
}

func (p *Provider) start(ctx context.Context) error {
	file, err := ioutil.ReadFile(p.initConfig.ConfigPath)
	if err != nil {
		return err
	}
	p.config = &config{}
	err = json.Unmarshal(file, p.config)
	if err != nil {
		return err
	}
	db, err := bitcask.Open(filepath.Join(p.config.WorkDir, DbPath))
	if err != nil {
		return err
	}
	go func() {
		select {
		case <-ctx.Done():
			db.Close()
		}
	}()

	p.db = db
	p.imageManager = NewImageManager(filepath.Join(p.config.WorkDir, ImagePath), db, p.config.MaxTimeout)
	record := p.initConfig.ResourceManager.GetRecord(ctx, "", "ProcessManager")
	p.processManager = NewProcessManager(filepath.Join(p.config.WorkDir, ContainerPath), db, p.imageManager, record,
		p.initConfig.InternalIP, p.initConfig.ResourceManager)
	p.podHandler = &PodEventHandler{
		HostIp: p.initConfig.InternalIP,
		events: make(chan ProcessEvent),
	}
	p.nodeHandler = &NodeEventHandler{
		p:      p,
		events: make(chan ProcessEvent),
	}
	return p.processManager.Start(ctx)
}

func NewProvider(ctx context.Context, cfg provider.InitConfig) (*Provider, error) {
	p := &Provider{
		initConfig: cfg,
		startTime:  time.Now(),
	}
	if err := p.start(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

func addAttributes(ctx context.Context, span trace.Span, attrs ...string) context.Context {
	if len(attrs)%2 == 1 {
		return ctx
	}
	for i := 0; i < len(attrs); i += 2 {
		ctx = span.WithField(ctx, attrs[i], attrs[i+1])
	}
	return ctx
}
