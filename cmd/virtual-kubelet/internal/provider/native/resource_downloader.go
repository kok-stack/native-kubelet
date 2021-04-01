package native

import "fmt"

func downloadResource(p *ContainerProcess, podProc *PodProcess, containerWorkDir string) error {
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
