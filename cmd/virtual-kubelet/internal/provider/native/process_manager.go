package native

import (
	"context"
	"fmt"
	"github.com/flytam/filenamify"
	"github.com/prologic/bitcask"
	"github.com/shirou/gopsutil/process"
	corev1 "k8s.io/api/core/v1"
	"path/filepath"
)

type ProcessManager struct {
	workdir   string
	processDb *bitcask.Bitcask
	run       chan *corev1.Pod
}

func NewProcessManager(workdir string, processDb *bitcask.Bitcask) *ProcessManager {
	return &ProcessManager{workdir: workdir, processDb: processDb}
}

func (m *ProcessManager) Start(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				close(m.run)
				return
			case p := <-m.run:
				go m.runPod(ctx, p)
			}
		}
	}()
	go func() {
		processes, err := process.ProcessesWithContext(ctx)
		if err != nil {
			panic(err)
		}
		for i, p := range processes {

			p.Cmdline()
		}

	}()

}

func (m *ProcessManager) runPod(ctx context.Context, p *corev1.Pod) {
	//转换pod为进程
	proc, err := m.convertPod2Process(ctx, p)
	if err != nil {
		//TODO: 转换pod出现错误,将错误使用record记录到pod中
	}
	proc.run(ctx)

}

//TODO:json tag
type PodProcess struct {
	workdir   string
	podName   string
	namespace string

	processChain []*ContainerProcess
	index        int
}

//如何记录index?如何更新pod?eventbus?
func (p *PodProcess) run(ctx context.Context) {
	for _, proc := range p.processChain {
		proc.run(ctx)
	}
}

type ContainerProcess struct {
	workdir string

	podName       string
	namespace     string
	containerName string
	image         string
	sync          bool
	RestartPolicy corev1.RestartPolicy

	im *ImageManager

	//status
}

/**
将pod转换为进程
*/
func (m *ProcessManager) convertPod2Process(ctx context.Context, p *corev1.Pod) (proc *PodProcess, err error) {
	dir, err := getProcessWorkDir(m.workdir, p)
	if err != nil {
		return nil, err
	}
	proc = &PodProcess{
		workdir:   dir,
		podName:   p.Name,
		namespace: p.Namespace,
	}
	proc.processChain = make([]*ContainerProcess, len(p.Spec.InitContainers)+len(p.Spec.Containers))
	for _, c := range p.Spec.InitContainers {
		dir, err = getContainerProcessWorkDir(m.workdir, p, c)
		if err != nil {
			return nil, err
		}
		proc.processChain = append(proc.processChain, NewProcess(p, c, dir))
	}
	for _, c := range p.Spec.Containers {
		dir, err = getContainerProcessWorkDir(m.workdir, p, c)
		if err != nil {
			return nil, err
		}
		proc.processChain = append(proc.processChain, NewProcess(p, c, dir))
	}
	return proc, nil
}

func (p *ContainerProcess) run(ctx context.Context) error {
	f := func() error {
		if err := p.im.PullImage(ctx, PullImageOpts{
			SrcImage: dockerImageName(p.image),
			//DockerAuthConfig:            nil,
			//DockerBearerRegistryToken:   "",
			//DockerRegistryUserAgent:     "",
			//DockerInsecureSkipTLSVerify: 0,
			//Timeout:                     0,
			//RetryCount:                  0,
		}); err != nil {
			return err
		}
		if err := p.im.decompressionImage(ctx, p.image, p.workdir); err != nil {
			return err
		}
		//command := exec.Command()
		//TODO:启动进程

		return nil
	}
	if p.sync {
		return f()
	} else {
		go f()
	}
	return nil
}

func NewProcess(pod *corev1.Pod, c corev1.Container, workdir string) *ContainerProcess {
	//TODO:运行命令，环境变量等参数设置
	return &ContainerProcess{
		workdir:       workdir,
		podName:       pod.Name,
		namespace:     pod.Namespace,
		containerName: c.Name,
		RestartPolicy: pod.Spec.RestartPolicy,
	}
}

func getProcessKey(process *ContainerProcess) []byte {
	return []byte(fmt.Sprintf("process:%s:%s", process.namespace, process.podName))
}

func getProcessWorkDir(workdir string, pod *corev1.Pod) (string, error) {
	join := filepath.Join(workdir, pod.Namespace, pod.Name)
	s, err := filenamify.Filenamify(join, filenamify.Options{Replacement: "-"})
	if err != nil {
		return "", err
	}
	return s, nil
}

func getContainerProcessWorkDir(workdir string, pod *corev1.Pod, c corev1.Container) (string, error) {
	dir, err := getProcessWorkDir(workdir, pod)
	if err != nil {
		return "", err
	}

	join := filepath.Join(dir, c.Name)
	s, err := filenamify.Filenamify(join, filenamify.Options{Replacement: "-"})
	if err != nil {
		return "", err
	}
	return s, nil
}
