package native

import (
	"context"
	"fmt"
	log2 "github.com/kok-stack/native-kubelet/log"
	"github.com/kok-stack/native-kubelet/trace"
	"github.com/shirou/gopsutil/process"
	"k8s.io/kubernetes/pkg/kubelet/kubeletconfig/util/log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func runProcess(ctx context.Context, span trace.Span, p *ContainerProcess, podProc *PodProcess, containerWorkDir string, index int, signal chan error) {
	ctx, span = trace.StartSpan(ctx, "runProcess")
	ctx = span.WithFields(ctx, log2.Fields{
		"workdir": containerWorkDir,
		"index":   index,
	})
	defer span.End()
	span.Logger().Debugf("开始运行进程 %s ,参数: %s", p.Container.Command, p.Container.Args)
	//pid不为0,且进程存在
	if p.Pid != 0 {
		exists, err := process.PidExists(int32(p.Pid))
		if err != nil {
			span.Logger().Errorf("检查pid是否存在出现错误,error:%v", err)
			span.SetStatus(err)
			signal <- err
			return
		}
		if exists {
			span.Logger().Debugf("进程已存在,跳过运行进程")
			signal <- nil
			return
		}
	}
	args := append(p.Container.Command, p.Container.Args...)
	span.Logger().Debugf("运行进程完整命令为 /bin/sh -c %s", strings.Join(args, " "))
	cmd := exec.Command("/bin/sh", "-c", strings.Join(args, " "))
	//开启新的进程组
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	envs, err := p.buildContainerEnvs(podProc.Pod)
	if err != nil {
		span.Logger().Errorf("构建容器环境变量错误,error:%v", err)
		span.SetStatus(err)
		signal <- err
		return
	}
	cmd.Env = envs
	cmd.Dir = filepath.Join(containerWorkDir, p.Container.WorkingDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = strings.NewReader("")

	file, err := os.OpenFile(filepath.Join(containerWorkDir, STDOUT), os.O_CREATE|os.O_APPEND|os.O_RDWR, os.ModePerm)
	if err != nil {
		span.Logger().Errorf("打开标准输出错误,error:%v", err)
		span.SetStatus(err)
		signal <- err
		return
	}
	defer func() {
		file.Sync()
		file.Close()
	}()
	cmd.Stdout = file
	cmd.Stderr = file
	err = cmd.Start()
	var msg string
	if err != nil {
		span.Logger().Errorf("启动进程错误,error:%v", err)
		span.SetStatus(err)
		msg = fmt.Sprintf("start cmd error image:%s,workdir:%s,cmd:%s,args:%v,err:%v",
			p.Container.Image, cmd.Dir, p.Container.Command, p.Container.Args, err)
	} else {
		p.Pid = cmd.Process.Pid
		span.Logger().Debug("已启动进程")
		msg = fmt.Sprintf("start cmd with Pid %v image:%s,workdir:%s,cmd:%s,args:%v",
			cmd.Process.Pid, p.Container.Image, cmd.Dir, p.Container.Command, p.Container.Args)
	}
	bus <- ContainerProcessRun{
		ProcessEvent: BaseProcessEvent{
			p:   podProc,
			cp:  p,
			err: err,
			msg: msg,
		},
		pid:   p.Pid,
		index: index,
	}
	signal <- err
	//if err == nil {
	//	if err := cmd.Wait(); err != nil {
	//		span.Logger().Errorf("等待进程结束错误:%v", err)
	//	}
	//}
}

func processWaiter(parentCtx context.Context, p *ContainerProcess, podProc *PodProcess, index int) {
	ctx, span := trace.StartSpan(parentCtx, "processWaiter")
	ctx = span.WithField(ctx, "pid", p.Pid)
	defer span.End()
	state, err := waitProcessDead(p)

	var msg string
	if err != nil {
		span.Logger().Errorf("等待进程结束出现错误,error:%v", err)
		span.SetStatus(err)
		//TODO:错误处理
		log.Errorf("run cmd end image:%s,err:%v", p.Container.Image, err)
		msg = fmt.Sprintf("run cmd end image:%s,err:%v", p.Container.Image, err)
	} else {
		if state != nil {
			msg = fmt.Sprintf("run cmd end image:%s,exitCode:%v,resion:%s", p.Container.Image, state.ExitCode(), state.String())
		}
	}

	bus <- ContainerProcessFinish{
		ProcessEvent: BaseProcessEvent{
			p:   podProc,
			cp:  p,
			msg: msg,
			err: err,
		},
		pid:   p.Pid,
		index: index,
		state: state,
	}
	span.Logger().Debug("运行进程完成")
}

func waitProcessDead(p *ContainerProcess) (*os.ProcessState, error) {
	proc, err := os.FindProcess(p.Pid)
	if err != nil {
		return nil, err
	}
	wait, err := proc.Wait()
	if err != nil {
		return nil, err
	}
	return wait, nil
}
