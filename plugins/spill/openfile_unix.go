//go:build unix

package spill

import (
	"fmt"
	"os"
	"syscall"
)

// noFollowFlag 是「不跟随符号链接」的打开标志。
//
// 为什么需要：落盘目录里出现一个「合法 id 名的符号链接」时（默认目录
// <tmp>/mcp-toolify/spill 是可预测路径，别人可以先放好软链），跟随它就把目录外的
// 文件读出来了，下载端点于是成了任意文件读取。os.Root 只拦「逃出根」的软链，
// 指向根内文件的相对软链它照样跟随，所以这个标志仍然必要。
// 调用方还会再判一次 Mode().IsRegular()：三道保险（Root + O_NOFOLLOW + IsRegular），
// 任一失效都不至于泄漏。
const noFollowFlag = syscall.O_NOFOLLOW

// dirOwnedByUs 判断落盘目录是否属于当前用户。
//
// 不属于自己的目录即使权限是 0700 也不安全：拥有者随时能改权限、换软链、删文件。
// 这是「别人先在 /tmp 建好目录」这类预创建攻击的另一半判据。
func dirOwnedByUs(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true // 拿不到 uid 就不阻断：权限位那道检查仍然生效
	}
	return int(st.Uid) == os.Geteuid()
}

// fileIdentity 返回一个文件/目录的 dev+ino（唯一身份）。
//
// 用途见 store.checkDirIdentity：启动时记下落盘目录的身份，之后每轮 GC 与当前路径
// 对账一次，用来发现「有人把落盘目录挪走/换成软链」——这是当前实现下这类动作的
// 唯一可观测信号（写入本身不受影响，因此完全静默）。
func fileIdentity(info os.FileInfo) (dev, ino uint64, ok bool) {
	st, sok := info.Sys().(*syscall.Stat_t)
	if !sok {
		return 0, 0, false
	}
	return uint64(st.Dev), st.Ino, true
}

// checkNotHardLink 在文件有多个硬链接时报错。
//
// spill 自己产出的文件恒为 1 个链接（O_EXCL 新建、从不 Link），所以 Nlink > 1 只可能
// 是别人在落盘目录里 os.Link 了一个目录外的文件进来 —— 结构上它与真 spill 文件无法
// 区分（软链那道 Lstat 判定也拦不住），只要属主字段对得上就会被吐出去。
//
// 这不构成额外风险：能在这个 0700 目录里建硬链接的人已经是服务用户，那时他本来就能
// 直接读结果文件。纯属多收一层，成本是每次下载一次已有的 Lstat 里读一个字段。
func checkNotHardLink(name string, info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink <= 1 {
		return nil
	}
	return fmt.Errorf("spill 文件 %s 有 %d 个硬链接，拒绝读取："+
		"本插件产出的文件恒为 1 个", name, st.Nlink)
}
