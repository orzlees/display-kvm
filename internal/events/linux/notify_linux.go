package linux

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/orzlee/displayKvm/internal/model"
)

type notifyListener struct {
	path, dir  string
	conn       *net.UnixConn
	once       sync.Once
	cleanupErr error
}

func newNotifyListener() (*notifyListener, error) {
	dir, err := os.MkdirTemp("", "displaykvm-logind-")
	if err != nil {
		return nil, model.NewError(model.CodeIO, "无法创建本次订阅通知目录", err)
	}
	n := &notifyListener{dir: dir, path: filepath.Join(dir, "notify")}
	n.conn, err = net.ListenUnixgram("unixgram", &net.UnixAddr{Name: n.path, Net: "unixgram"})
	if err != nil {
		return nil, model.NewError(model.CodeIO, "无法创建订阅通知socket", errors.Join(err, n.close()))
	}
	raw, err := n.conn.SyscallConn()
	if err == nil {
		var optionErr error
		err = raw.Control(func(fd uintptr) {
			optionErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_PASSCRED, 1)
		})
		if err == nil {
			err = optionErr
		}
	}
	if err != nil {
		return nil, model.NewError(model.CodeIO, "无法启用订阅通知发送者凭据校验", errors.Join(err, n.close()))
	}
	return n, nil
}

func (n *notifyListener) wait(ctx context.Context, pid int) error {
	data, control := make([]byte, 4096), make([]byte, syscall.CmsgSpace(syscall.SizeofUcred))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := n.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			return model.NewError(model.CodeIO, "订阅通知socket已经关闭", err)
		}
		count, oob, flags, _, err := n.conn.ReadMsgUnix(data, control)
		if err != nil {
			if e, ok := err.(net.Error); ok && e.Timeout() {
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return model.NewError(model.CodeIO, "读取订阅就绪通知失败", err)
		}
		if flags&(syscall.MSG_TRUNC|syscall.MSG_CTRUNC) != 0 {
			continue
		}
		messages, err := syscall.ParseSocketControlMessage(control[:oob])
		if err != nil {
			continue
		}
		for _, message := range messages {
			credentials, err := syscall.ParseUnixCredentials(&message)
			// 私有目录只隔离路径；仍必须核对内核提供的PID，不能相信通知正文自报身份。
			if err == nil && validNotification(data[:count], int(credentials.Pid), pid) {
				return nil
			}
		}
	}
}

func (n *notifyListener) close() error {
	n.once.Do(func() {
		var failures []error
		if n.conn != nil {
			if err := n.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				failures = append(failures, err)
			}
		}
		// 取消与进程退出可并发触发清理，已关闭或已删除属于幂等成功；其他错误
		// 必须带回Run，避免把遗留socket/目录当作已完成释放。
		for _, target := range []string{n.path, n.dir} {
			if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
				failures = append(failures, err)
			}
		}
		if err := errors.Join(failures...); err != nil {
			n.cleanupErr = model.NewError(model.CodeIO, "清理订阅通知资源失败", err)
		}
	})
	return n.cleanupErr
}
