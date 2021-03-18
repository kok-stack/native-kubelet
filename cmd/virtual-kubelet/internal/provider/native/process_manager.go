package native

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/dockershim"
	"time"
)

type ProcessManager struct {
	imageManager     *ImageManager
	containerManager *ContainerManager
}

func (m *ProcessManager) create(ctx context.Context, pod *corev1.Pod) error {
	//拉取image,解压,运行
	images := getImages(pod)
	if err := pullImages(ctx, m.imageManager, images); err != nil {
		return err
	}
	if err := runInitContainers(ctx, m.imageManager, m.containerManager, pod); err != nil {
		return err
	}
	//运行
	if err := runContainers(ctx, m.imageManager, m.containerManager, pod); err != nil {
		return err
	}
}

//普通容器,并发运行
func runContainers(ctx context.Context, im *ImageManager, cm *ContainerManager, pod *corev1.Pod) error {
	for i, c := range pod.Spec.Containers {
		if err := cm.createContainer(ctx, im, c, pod); err != nil {
			return err
		}
	}
	return nil
}

/*
1.创建container
2.解压到container
3.run container
*/
//init 容器按照顺序运行
func runInitContainers(ctx context.Context, im *ImageManager, cm *ContainerManager, pod *corev1.Pod) error {
	for i, c := range pod.Spec.InitContainers {
		if err := cm.createContainer(ctx, im, c, pod); err != nil {
			return err
		}
	}
	return nil
}

func pullImages(ctx context.Context, manager *ImageManager, images []string) error {
	//TODO:支持docker镜像ImagePullSecrets
	for _, image := range images {
		if err := manager.PullImage(ctx, PullImageOpts{
			SrcImage:                    dockerImageName(image),
			DockerAuthConfig:            nil,
			DockerBearerRegistryToken:   "",
			DockerRegistryUserAgent:     "",
			DockerInsecureSkipTLSVerify: 0,
			Timeout:                     time.Hour,
			RetryCount:                  10,
		}); err != nil {
			return err
		}
	}
	return nil
}

func dockerImageName(image string) string {
	return dockershim.DockerImageIDPrefix + image
}

func getImages(pod *corev1.Pod) []string {
	images := make([]string, 0)
	for _, c := range pod.Spec.InitContainers {
		images = append(images, c.Image)
	}
	for _, c := range pod.Spec.Containers {
		images = append(images, c.Image)
	}
	return images
}

func newProcessManager(im *ImageManager) *ProcessManager {
	return &ProcessManager{
		imageManager: im,
	}
}
