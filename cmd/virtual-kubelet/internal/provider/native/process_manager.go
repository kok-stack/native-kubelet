package native

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/flytam/filenamify"
	"github.com/prologic/bitcask"
	corev1 "k8s.io/api/core/v1"
	"path/filepath"
)

type ProcessManager struct {
	workdir   string
	processDb *bitcask.Bitcask
}

func NewProcessManager(workdir string, processDb *bitcask.Bitcask) *ProcessManager {
	return &ProcessManager{workdir: workdir, processDb: processDb}
}

type KubeletProcess struct {
	workdir   string
	podName   string
	namespace string
}

func (p *KubeletProcess) Run(ctx context.Context) error {
	//command := exec.Command()
}

func (m *ProcessManager) createProcess(ctx context.Context, im *ImageManager, c corev1.Container, pod *corev1.Pod) (*KubeletProcess, error) {
	//获取解压的地址
	workdir, err := getProcessWorkDir(m.workdir, pod, c)
	if err != nil {
		return nil, err
	}
	//解压
	err = im.decompressionImage(ctx, c.Image, workdir)
	if err != nil {
		return nil, err
	}
	//配置运行process
	return NewProcess(pod, c, workdir), nil
}

/*
1.创建process
2.解压到process
3.run process
*/
func (m *ProcessManager) createPersistenceProcess(ctx context.Context, im *ImageManager, c corev1.Container, pod *corev1.Pod) (*KubeletProcess, error) {
	proc, err := m.createProcess(ctx, im, c, pod)
	if err != nil {
		return proc, nil
	}
	marshal, err := json.Marshal(proc)
	if err != nil {
		return nil, err
	}
	err = m.processDb.Put(getProcessKey(proc), marshal)
	if err != nil {
		return nil, err
	}
	return proc, nil
}

func NewProcess(pod *corev1.Pod, c corev1.Container, workdir string) *KubeletProcess {
	//TODO:运行命令，环境变量等参数设置
	return &KubeletProcess{
		workdir:   workdir,
		podName:   pod.Name,
		namespace: pod.Namespace,
	}
}

func getProcessKey(process *KubeletProcess) []byte {
	return []byte(fmt.Sprintf("process:%s:%s", process.namespace, process.podName))
}

func getProcessWorkDir(workdir string, pod *corev1.Pod, c corev1.Container) (string, error) {
	join := filepath.Join(workdir, fmt.Sprintf("%-%-%s", pod.Namespace, pod.Name, c.Name))
	s, err := filenamify.Filenamify(join, filenamify.Options{Replacement: "-"})
	if err != nil {
		return "", err
	}
	return s, nil
}
