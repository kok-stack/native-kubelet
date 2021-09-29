package native

import (
	"context"
	"fmt"
	"github.com/kok-stack/native-kubelet/log"
	"github.com/kok-stack/native-kubelet/trace"
	"io/ioutil"
	corev1 "k8s.io/api/core/v1"
	ers "k8s.io/apimachinery/pkg/api/errors"
	"os"
	"path/filepath"
)

func downloadResource(parentCtx context.Context, p *ContainerProcess, podProc *PodProcess, containerWorkDir string) error {
	ctx, span := trace.StartSpan(parentCtx, "downloadResource")
	ctx = span.WithFields(ctx, log.Fields{
		"p.ConfigMapDownload": p.ConfigMapDownload,
		"p.SecretDownload":    p.SecretDownload,
	})
	defer span.End()
	if !p.ConfigMapDownload {
		if err := p.downloadConfigMaps(podProc.Pod, containerWorkDir); err != nil {
			bus <- DownloadResource{
				BaseProcessEvent{
					p:   podProc,
					err: err,
					cp:  p,
					msg: fmt.Sprintf("download configmap error image:%s,err:%v", p.Container.Image, err),
				},
			}
			return err
		} else {
			p.ConfigMapDownload = true
			bus <- DownloadResource{
				BaseProcessEvent{
					p:   podProc,
					cp:  p,
					msg: fmt.Sprintf("download configmap finish image:%s", p.Container.Image),
				},
			}
		}
	}
	if !p.SecretDownload {
		if err := p.downloadSecrets(podProc.Pod, containerWorkDir); err != nil {
			bus <- DownloadResource{
				BaseProcessEvent{
					p:   podProc,
					err: err,
					cp:  p,
					msg: fmt.Sprintf("download secret error image:%s,err:%v", p.Container.Image, err),
				},
			}
			return err
		} else {
			p.SecretDownload = true
			bus <- DownloadResource{
				BaseProcessEvent{
					p:   podProc,
					cp:  p,
					msg: fmt.Sprintf("download secret finish image:%s", p.Container.Image),
				},
			}
		}
	}
	return nil
}

func (p *ContainerProcess) downloadConfigMaps(pod *corev1.Pod, workdir string) error {
	namespace := pod.Namespace
	for _, volume := range pod.Spec.Volumes {
		if volume.ConfigMap != nil {
			name := volume.ConfigMap.Name
			cm, err := p.pm.resourceManager.GetConfigMap(name, namespace)
			if err != nil {
				if ers.IsNotFound(err) && *volume.ConfigMap.Optional {
					continue
				}
				return err
			}
			var paths []corev1.KeyToPath
			if len(volume.ConfigMap.Items) == 0 {
				paths = make([]corev1.KeyToPath, 0, len(cm.Data))
				for k := range cm.Data {
					paths = append(paths, corev1.KeyToPath{
						Key:  k,
						Path: k,
					})
				}
			} else {
				paths = volume.ConfigMap.Items
			}
			for _, item := range paths {
				bytes, ok := cm.Data[item.Key]
				if !ok {
					if volume.ConfigMap.Optional != nil && *(volume.ConfigMap.Optional) {
						continue
					} else {
						return fmt.Errorf("not found volume configmap %s key %s in namespace %s", name, item.Key, namespace)
					}
				}
				mountPath := item.Path
				for _, mount := range p.Container.VolumeMounts {
					if mount.Name == volume.Name && mount.SubPath == item.Path {
						mountPath = mount.MountPath
						break
					}
				}

				err := ioutil.WriteFile(filepath.Join(workdir, mountPath), []byte(bytes), os.ModePerm)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (p *ContainerProcess) downloadSecrets(pod *corev1.Pod, workdir string) error {
	namespace := pod.Namespace
	for _, volume := range pod.Spec.Volumes {
		if volume.Secret != nil {
			name := volume.Secret.SecretName
			sec, err := p.pm.resourceManager.GetSecret(name, namespace)
			if err != nil {
				if ers.IsNotFound(err) && *volume.Secret.Optional {
					continue
				}
				return err
			}
			var paths []corev1.KeyToPath
			if len(volume.Secret.Items) == 0 {
				paths = make([]corev1.KeyToPath, 0, len(sec.Data))
				for k := range sec.Data {
					paths = append(paths, corev1.KeyToPath{
						Key:  k,
						Path: k,
					})
				}
			} else {
				paths = volume.Secret.Items
			}
			for _, item := range paths {
				s, ok := sec.Data[item.Key]
				if !ok {
					if volume.Secret.Optional != nil && *volume.Secret.Optional {
						continue
					} else {
						return fmt.Errorf("not found volume secret %s key %s in namespace %s", name, item.Key, namespace)
					}
				}
				mountPath := item.Path
				for _, mount := range p.Container.VolumeMounts {
					if mount.Name == volume.Name && mount.SubPath == item.Path {
						mountPath = mount.MountPath
						break
					}
				}

				err := ioutil.WriteFile(filepath.Join(workdir, mountPath), s, os.ModePerm)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}
