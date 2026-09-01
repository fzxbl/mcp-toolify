//go:build !unix

package spill

import (
	"os"
)

// noFollowFlag 在非 unix 平台上没有对应标志，取 0。
//
// 这些平台上「目录里被塞了一个 id 形状的软链」这条只能靠 os.Root（逃出根的软链会
// 被拦下）与调用方的 Mode().IsRegular() 二次校验兜住；指向根内文件的相对软链
// 仍可能被跟随，属于已知的平台差异（本仓库的部署目标是 linux）。
const noFollowFlag = 0

// dirOwnedByUs 在非 unix 平台上拿不到 uid，恒为真：权限位那道检查仍然生效。
func dirOwnedByUs(os.FileInfo) bool { return true }

// fileIdentity 在非 unix 平台上拿不到 dev+ino，返回 ok=false：
// store 会跳过「落盘目录被换手」的对账告警（只是少一条可观测信号，不影响落盘）。
func fileIdentity(os.FileInfo) (dev, ino uint64, ok bool) { return 0, 0, false }

// checkNotHardLink 在非 unix 平台上拿不到 Nlink，恒放行。
//
// 也就是说这些平台上「目录内硬链接指向目录外的合法 owner 文件」这一形态挡不住。
// 前提同 unix 分支：攻击者已经是服务用户，那时他本可直接读结果文件，
// 因此不构成额外风险（本仓库的部署目标是 linux）。
func checkNotHardLink(string, os.FileInfo) error { return nil }
