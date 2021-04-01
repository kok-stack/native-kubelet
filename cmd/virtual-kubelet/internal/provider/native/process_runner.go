package native

import (
	"fmt"
	"github.com/kok-stack/native-kubelet/trace"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func runProcess(span trace.Span, p *ContainerProcess, podProc *PodProcess, containerWorkDir string, index int, signal chan error) {
	span.Logger().Debug("开始运行进程", p.Container.Command, "args:", p.Container.Args)
	if p.Pid == 0 && !p.PidDead {
		args := append(p.Container.Command, p.Container.Args...)
		span.Logger().Debug("运行进程完整命令为", "/bin/sh -c", strings.Join(args, " "))
		cmd := exec.Command("/bin/sh", "-c", strings.Join(args, " "))
		envs, err := p.buildContainerEnvs(podProc.Pod)
		if err != nil {
			signal <- err
			panic(err.Error())
		}
		cmd.Env = envs
		cmd.Dir = filepath.Join(containerWorkDir, p.Container.WorkingDir)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdin = strings.NewReader("")

		file, err := os.OpenFile(filepath.Join(containerWorkDir, STDOUT), os.O_CREATE|os.O_APPEND|os.O_RDWR, os.ModePerm)
		if err != nil {
			signal <- err
			panic(err.Error())
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
			msg = fmt.Sprintf("start cmd error image:%s,workdir:%s,cmd:%s,args:%v,err:%v",
				p.Container.Image, cmd.Dir, p.Container.Command, p.Container.Args, err)
		} else {
			p.Pid = cmd.Process.Pid
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
		if err == nil {
			if err := cmd.Wait(); err != nil {
				panic(err.Error())
			}
		}
	}
}

func processWaiter(p *ContainerProcess, podProc *PodProcess, index int, span trace.Span) error {
	state, err := waitProcessDead(p)

	p.PidDead = true
	var msg string
	if state != nil {
		msg = fmt.Sprintf("run cmd end image:%s,exitCode:%v,resion:%s", p.Container.Image, state.ExitCode(), state.String())
	} else {
		msg = fmt.Sprintf("run cmd end image:%s,err:%v", p.Container.Image, err)
	}
	bus <- ContainerProcessFinish{
		ProcessEvent: BaseProcessEvent{
			p:   podProc,
			cp:  p,
			msg: msg,
		},
		pid:   p.Pid,
		index: index,
		state: state,
	}
	span.Logger().Debug("运行进程完成", p.Container.Command)
	return err
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
